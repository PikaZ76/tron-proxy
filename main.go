package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
)

type JSONRPCRequest struct {
	Jsonrpc string            `json:"jsonrpc"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
	ID      interface{}       `json:"id"`
}

type JSONRPCResponse struct {
	Jsonrpc string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   interface{} `json:"error,omitempty"`
}

var (
	tronJSONRPCEndpoint = os.Getenv("TRON_JSONRPC_ENDPOINT")
	tronRestEndpoint    = os.Getenv("TRON_REST_ENDPOINT")
	traceDir         = "/project/trace"
)

func main() {

	// 设置log前缀和输出选项
	log.SetPrefix("[proxy] ")
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	http.HandleFunc("/jsonrpc", handleJSONRPC)
	http.HandleFunc("/test", handleTest)
	log.Println("Proxy server started on :9090")
	http.ListenAndServe(":9090", nil)
}

func handleTest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
}

func handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		sendError(w, nil, -32603, "Internal error: unable to read request body")
		return
	}

	// 打印原始请求体日志
	log.Printf("Incoming request body: %s", string(body))

	var raw interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		log.Printf("JSON parse error: %v", err)
		sendError(w, nil, -32700, "Parse error: invalid JSON")
		return
	}

	switch v := raw.(type) {
	case map[string]interface{}:
		// 单请求
		log.Println("Detected single JSON-RPC request")
		req, perr := parseSingleRequest(v)
		if perr != nil {
			log.Printf("Parse single request error: %v", perr)
			sendError(w, nil, -32700, "Parse error: invalid request object")
			return
		}
		resp := handleSingleRequest(req)
		sendJSONRPCResponse(w, resp)
		// 打印响应日志
		log.Printf("Single request response: %s", r.URL.Path)

	case []interface{}:
		// 批处理请求
		if len(v) == 0 {
			log.Println("Batch request but empty array, returning empty[]")
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
			return
		}
		log.Printf("Detected batch JSON-RPC request with %d items", len(v))

		reqs, errs := parseBatchRequests(v)
		if errs != nil {
			log.Printf("Batch parse error, items with parse fail: %d", len(errs))
			sendBatchResponse(w, errs)
			return
		}

		// 检查method一致
		allMethod := reqs[0].Method
		for _, r := range reqs {
			if r.Method != allMethod {
				log.Println("Mixed methods in batch not supported, returning error")
				sendBatchResponse(w, createErrorResponsesForBatch(reqs, -32601, "Mixed methods not supported"))
				return
			}
		}

		// 根据method分类处理
		var responses []JSONRPCResponse
		switch allMethod {
		case "debug_traceBlockByHash":
			log.Printf("Batch method getTransactionInfoByBlockNum, requests: %d", len(reqs))
			responses = handleBatchGetTransactionInfo(reqs)
		case "eth_debugTransactionTrace":
			log.Printf("Batch method eth_debugTransactionTrace, requests: %d", len(reqs))
			responses = handleBatchDebugTransactionTrace(reqs)
		default:
			log.Printf("Batch method %s not recognized (third category), directly forwarding batch", allMethod)
			responses = forwardBatchToJSONRPC(reqs, v, tronJSONRPCEndpoint)
		}

		sendBatchResponse(w, responses)
		// 打印批处理响应日志
		log.Printf("Batch request response items: %d", len(responses))

	default:
		log.Println("Invalid JSON structure, not single or batch array")
		sendError(w, nil, -32700, "Parse error: invalid structure")
	}
}

func parseSingleRequest(v map[string]interface{}) (JSONRPCRequest, error) {
	reqBytes, _ := json.Marshal(v)
	var req JSONRPCRequest
	if err := json.Unmarshal(reqBytes, &req); err != nil {
		return JSONRPCRequest{}, err
	}
	return req, nil
}

func parseBatchRequests(arr []interface{}) ([]JSONRPCRequest, []JSONRPCResponse) {
	reqs := make([]JSONRPCRequest, 0, len(arr))
	var errors []JSONRPCResponse
	for _, elem := range arr {
		elemBytes, _ := json.Marshal(elem)
		var req JSONRPCRequest
		if err := json.Unmarshal(elemBytes, &req); err != nil {
			errors = append(errors, JSONRPCResponse{
				Jsonrpc: "2.0",
				ID:      nil,
				Error: map[string]interface{}{
					"code":    -32700,
					"message": "Parse error: invalid JSON in batch",
				},
			})
			continue
		}
		reqs = append(reqs, req)
	}
	if len(errors) > 0 {
		return nil, errors
	}
	return reqs, nil
}

func handleSingleRequest(req JSONRPCRequest) JSONRPCResponse {
	log.Printf("handleSingleRequest - method=%s, id=%v", req.Method, req.ID)
	if req.Jsonrpc != "2.0" {
		return jsonError(req.ID, -32600, "Invalid Request")
	}

	switch req.Method {
	case "debug_traceBlockByHash":
		return handleGetTransactionInfoByBlockNum(req)
	case "eth_debugTransactionTrace":
		return handleDebugTransactionTrace(req)
	default:
		// 透传到下游
		return forwardAndReturn(req, tronJSONRPCEndpoint)
	}
}

type TronTransactionInfo struct {
	InternalTransactions []json.RawMessage `json:"internal_transactions"`
	Id                   string            `json:"id"`
	BlockNumber          int64             `json:"blockNumber"`
	TransactionHash      string            `json:"transactionHash"`
}

func handleGetTransactionInfoByBlockNum(req JSONRPCRequest) JSONRPCResponse {
	if len(req.Params) == 0 {
		return jsonError(req.ID, -32602, "Invalid params")
	}
	var blockId int64
	if err := json.Unmarshal(req.Params[0], &blockId); err != nil {
		return jsonError(req.ID, -32602, "Invalid params: must be integer block number")
	}

	postData := map[string]interface{}{
		"num": blockId,
	}
	postBytes, _ := json.Marshal(postData)
	log.Printf("REST call for blockNum=%d", blockId)
	txInfoBlockUrl := tronRestEndpoint + "/wallet/gettransactioninfobyblocknum"
	resp, err := http.Post(txInfoBlockUrl, "application/json", bytes.NewReader(postBytes))
	if err != nil {
		log.Printf("REST request error: %v", err)
		return jsonError(req.ID, -32603, "Internal error: "+err.Error())
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	log.Printf("REST response code=%d, body=%s", resp.StatusCode, string(respBody))

	var respJson []TronTransactionInfo
	if err := json.Unmarshal(respBody, &respJson); err != nil {
		return jsonError(req.ID, -32603, "Invalid response from TronNode REST")
	}

	return JSONRPCResponse{
		Jsonrpc: "2.0",
		ID:      req.ID,
		Result:  respJson,
	}
}

func handleDebugTransactionTrace(req JSONRPCRequest) JSONRPCResponse {
	if len(req.Params) == 0 {
		return jsonError(req.ID, -32602, "Invalid params")
	}
	var txId string
	if err := json.Unmarshal(req.Params[0], &txId); err != nil {
		return jsonError(req.ID, -32602, "Invalid params: must be string TxId")
	}

	log.Printf("Reading trace file for txId=%s", txId)
	filePath := fmt.Sprintf("%s/%s.json", traceDir, txId)
	fileData, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("Error reading file: %v", err)
		return jsonError(req.ID, -32603, "cannot read trace file")
	}

	var traceJson interface{}
	if err := json.Unmarshal(fileData, &traceJson); err != nil {
		return jsonError(req.ID, -32603, "Invalid JSON in trace file")
	}

	return JSONRPCResponse{
		Jsonrpc: "2.0",
		ID:      req.ID,
		Result:  traceJson,
	}
}

func forwardAndReturn(req JSONRPCRequest, targetURL string) JSONRPCResponse {
	log.Printf("Forwarding single request to %s, method=%s, id=%v", targetURL, req.Method, req.ID)
	reqBytes, _ := json.Marshal(req)
	resp, err := http.Post(targetURL, "application/json", bytes.NewReader(reqBytes))
	if err != nil {
		log.Printf("Forward request error: %v", err)
		return jsonError(req.ID, -32603, "Internal error: "+err.Error())
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	// log.Printf("Forwarded response code=%d, body=%s", resp.StatusCode, string(respBody))

	var forwardResp JSONRPCResponse
	if err := json.Unmarshal(respBody, &forwardResp); err != nil {
		return jsonError(req.ID, -32603, "Invalid response from forwarded service")
	}
	forwardResp.ID = req.ID
	return forwardResp
}

func handleBatchGetTransactionInfo(reqs []JSONRPCRequest) []JSONRPCResponse {
	responses := make([]JSONRPCResponse, len(reqs))
	for i, r := range reqs {
		responses[i] = handleGetTransactionInfoByBlockNum(r)
	}
	return responses
}

func handleBatchDebugTransactionTrace(reqs []JSONRPCRequest) []JSONRPCResponse {
	responses := make([]JSONRPCResponse, len(reqs))
	var wg sync.WaitGroup
	wg.Add(len(reqs))

	for i := range reqs {
		idx := i
		go func() {
			defer wg.Done()
			var txId string
			if len(reqs[idx].Params) == 0 {
				responses[idx] = jsonError(reqs[idx].ID, -32602, "Invalid params")
				return
			}
			if err := json.Unmarshal(reqs[idx].Params[0], &txId); err != nil {
				responses[idx] = jsonError(reqs[idx].ID, -32602, "Invalid params: must be string TxId")
				return
			}
			log.Printf("Reading trace file(batch) for txId=%s", txId)

			filePath := fmt.Sprintf("%s/%s.json", traceDir, txId)
			fileData, err := os.ReadFile(filePath)
			if err != nil {
				log.Printf("Error reading file in batch: %v", err)
				responses[idx] = jsonError(reqs[idx].ID, -32603, "cannot read trace file")
				return
			}

			var traceJson interface{}
			if err := json.Unmarshal(fileData, &traceJson); err != nil {
				responses[idx] = jsonError(reqs[idx].ID, -32603, "Invalid JSON in trace file")
				return
			}
			responses[idx] = JSONRPCResponse{
				Jsonrpc: "2.0",
				ID:      reqs[idx].ID,
				Result:  traceJson,
			}
		}()
	}

	wg.Wait()
	return responses
}

func forwardBatchToJSONRPC(reqs []JSONRPCRequest, originalArr []interface{}, targetURL string) []JSONRPCResponse {
	log.Printf("Forwarding batch request (length=%d) to %s", len(reqs), targetURL)
	originalBody, _ := json.Marshal(originalArr)
	resp, err := http.Post(targetURL, "application/json", bytes.NewReader(originalBody))
	if err != nil {
		log.Printf("Forward batch request error: %v", err)
		return createErrorResponsesForBatch(reqs, -32603, "Internal error: "+err.Error())
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	// log.Printf("Forwarded batch response code=%d, body=%s", resp.StatusCode, string(respBody))

	var batchResp []JSONRPCResponse
	if err := json.Unmarshal(respBody, &batchResp); err == nil {
		return batchResp
	}
	// 若无法解析为数组，尝试解析为单一Response
	var singleResp JSONRPCResponse
	if err := json.Unmarshal(respBody, &singleResp); err == nil && singleResp.ID != nil {
		return []JSONRPCResponse{singleResp}
	}
	// 否则返回错误
	return createErrorResponsesForBatch(reqs, -32603, "Invalid response from forwarded service")
}

func sendJSONRPCResponse(w http.ResponseWriter, resp JSONRPCResponse) {
	if resp.ID == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func sendBatchResponse(w http.ResponseWriter, responses []JSONRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responses)
}

func sendError(w http.ResponseWriter, id interface{}, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	resp := JSONRPCResponse{
		Jsonrpc: "2.0",
		ID:      id,
		Error: map[string]interface{}{
			"code":    code,
			"message": message,
		},
	}
	json.NewEncoder(w).Encode(resp)
}

func jsonError(id interface{}, code int, msg string) JSONRPCResponse {
	return JSONRPCResponse{
		Jsonrpc: "2.0",
		ID:      id,
		Error: map[string]interface{}{
			"code":    code,
			"message": msg,
		},
	}
}

func createErrorResponsesForBatch(reqs []JSONRPCRequest, code int, msg string) []JSONRPCResponse {
	responses := make([]JSONRPCResponse, len(reqs))
	for i, r := range reqs {
		responses[i] = jsonError(r.ID, code, msg)
	}
	return responses
}
