package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

type agentTurnStreamEvent struct {
	Type string `json:"type"`; Iteration int `json:"iteration,omitempty"`; MessageCount int `json:"messageCount,omitempty"`; ContextChars int `json:"contextChars,omitempty"`; FileDiffs any `json:"fileDiffs,omitempty"`; SessionID string `json:"sessionId,omitempty"`; Delta string `json:"delta,omitempty"`; Content string `json:"content,omitempty"`; ToolCallID string `json:"toolCallId,omitempty"`; ToolName string `json:"toolName,omitempty"`; ToolError bool `json:"toolError,omitempty"`; StopReason string `json:"stopReason,omitempty"`; Error string `json:"error,omitempty"`
}

type agentTurnStreamHook struct { agent.NopHook; ctx context.Context; events chan<- agentTurnStreamEvent }
func (h *agentTurnStreamHook) emit(event agentTurnStreamEvent) { select { case h.events <- event: case <-h.ctx.Done(): } }
func (h *agentTurnStreamHook) OnTextDelta(ctx context.Context, delta string){ h.emit(agentTurnStreamEvent{Type:"text_delta",Delta:delta}) }
func (h *agentTurnStreamHook) OnReasoningDelta(ctx context.Context, delta string){ h.emit(agentTurnStreamEvent{Type:"reasoning_delta",Delta:delta}) }
func (h *agentTurnStreamHook) OnReasoningEnd(ctx context.Context){ h.emit(agentTurnStreamEvent{Type:"reasoning_end"}) }
func (h *agentTurnStreamHook) OnToolStart(ctx context.Context, call core.ToolCall){ h.emit(agentTurnStreamEvent{Type:"tool_start",ToolCallID:call.ID,ToolName:call.Name}) }
func (h *agentTurnStreamHook) OnToolEnd(ctx context.Context, call core.ToolCall, result core.ToolResult){ h.emit(agentTurnStreamEvent{Type:"tool_end",ToolCallID:call.ID,ToolName:call.Name,ToolError:result.IsError,FileDiffs:result.FileDiffs}) }

func (s *Server) registerAgentTurnStream(mux *http.ServeMux) {
	mux.HandleFunc("/api/agent/turn/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost { w.Header().Set("Allow",http.MethodPost); http.Error(w,"Method not allowed",http.StatusMethodNotAllowed); return }
		loop:=s.loop.Load(); if loop==nil { http.Error(w,"Agent loop is not available",http.StatusServiceUnavailable); return }
		sessionID,message,media,ok:=decodeAgentTurnRequest(w,r); if !ok{return}
		var err error
		media,err=s.validateWebUIMedia(sessionID,media); if err!=nil { http.Error(w,"Invalid attachment: "+err.Error(),http.StatusBadRequest); return }
		flusher,ok:=w.(http.Flusher); if !ok { http.Error(w,"Streaming is not supported",http.StatusInternalServerError); return }
		w.Header().Set("Content-Type","application/x-ndjson; charset=utf-8"); w.Header().Set("Cache-Control","no-cache, no-store"); w.Header().Set("X-Accel-Buffering","no"); w.WriteHeader(http.StatusOK); flusher.Flush()
		ctx,cancel:=context.WithTimeout(r.Context(),180*time.Second); defer cancel()
		events:=make(chan agentTurnStreamEvent,64); resultCh:=make(chan struct{out *core.OutboundMessage; err error},1); hook:=&agentTurnStreamHook{ctx:ctx,events:events}
		temporary:=strings.HasPrefix(sessionID,"tmp_")
		go func(){
			defer close(events)
			if temporary { defer func(){ _=cleanupWebUIMedia(s.webUIMediaRoot(),sessionID) }() }
			metadata:=map[string]any{"source":"webui","session_id":sessionID}; if temporary { metadata["_temporary_chat"]=true }
			out,err:=loop.ProcessMessageWithHook(ctx,core.InboundMessage{Channel:"webui",SenderID:sessionID,ChatID:sessionID,Content:message,Media:media,Timestamp:time.Now(),Metadata:metadata},hook)
			resultCh<-struct{out *core.OutboundMessage; err error}{out:out,err:err}
		}()
		encoder:=json.NewEncoder(w); for event:=range events { if err:=encoder.Encode(event);err!=nil{return}; flusher.Flush() }
		result:=<-resultCh; if result.err!=nil { if err:=encoder.Encode(agentTurnStreamEvent{Type:"error",Error:result.err.Error()});err==nil{flusher.Flush()}; return }
		content:=""; if result.out!=nil{content=result.out.Content}; _=encoder.Encode(agentTurnStreamEvent{Type:"done",SessionID:sessionID,Content:content,StopReason:"completed"}); flusher.Flush()
	})
}
