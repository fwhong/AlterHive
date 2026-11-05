//go:build ignore

// Isolated integration fixture; never copied into the production image.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var mu sync.Mutex
var records []map[string]any

func main() {
	if len(os.Args) > 1 && os.Args[1] == "request" {
		// request socket-or-dash URL METHOD BODY TOKEN
		client := &http.Client{Timeout: 3 * time.Second}
		if os.Args[2] != "-" {
			client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", os.Args[2])
			}}
		}
		req, e := http.NewRequest(os.Args[4], os.Args[3], strings.NewReader(os.Args[5]))
		if e != nil {
			panic(e)
		}
		req.Header.Set("Content-Type", "application/json")
		if os.Args[6] != "" {
			req.Header.Set("Authorization", "Bearer "+os.Args[6])
		}
		resp, e := client.Do(req)
		if e != nil {
			fmt.Println(e)
			os.Exit(2)
		}
		defer resp.Body.Close()
		fmt.Println(resp.StatusCode)
		io.Copy(os.Stdout, resp.Body)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "read" {
		b, e := os.ReadFile(os.Args[2])
		if e != nil {
			fmt.Println(e)
			os.Exit(2)
		}
		os.Stdout.Write(b)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "env" {
		for _, s := range os.Environ() {
			fmt.Println(strings.SplitN(s, "=", 2)[0])
		}
		return
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/records" {
			mu.Lock()
			defer mu.Unlock()
			json.NewEncoder(w).Encode(records)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		records = append(records, map[string]any{"path": r.URL.Path, "auth": r.Header.Get("Authorization"), "body": string(b)})
		mu.Unlock()
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "mock-model"}}})
			return
		}
		var req struct {
			ResponseFormat map[string]any `json:"response_format"`
			Messages       []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(b, &req)
		content := "Nmap scan report for 192.168.56.60\nPORT STATE SERVICE\n22/tcp open ssh MOCKLLM-RENDER\n3306/tcp open mysql\n"
		if len(req.ResponseFormat) > 0 {
			content = `{"plan_id":"mock-plan","phase":"recon"}`
			if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "intent_type") {
				content = `{"intent_type":"network_recon","confidence":0.9}`
			}
			if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "WorldPatch") {
				content = `{"source":"llm_world_patch_agent","files":[{"path":"/tmp/mock-proof.txt","content":"MOCK-WORLD-PATCH"}]}`
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
	})
	if e := http.ListenAndServe(":9999", nil); e != nil {
		panic(e)
	}
}
