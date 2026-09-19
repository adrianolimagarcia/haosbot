package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/session"
)

func (s *Server) handleWebUITemporary(w http.ResponseWriter,r *http.Request){
	switch r.Method{
	case http.MethodPost:
		var raw [16]byte
		if _,err:=rand.Read(raw[:]);err!=nil{http.Error(w,err.Error(),500);return}
		id:="tmp_"+hex.EncodeToString(raw[:]);key:="webui:"+id
		if _,err:=session.DefaultTransientStore().Open(key);err!=nil{http.Error(w,err.Error(),500);return}
		writeWebUIJSON(w,map[string]any{"session_id":id,"key":key,"temporary":true})
	case http.MethodDelete:
		id:=strings.TrimSpace(r.URL.Query().Get("session_id"))
		if !strings.HasPrefix(id,"tmp_")||!webSessionIDPattern.MatchString(id){http.Error(w,"invalid temporary session",400);return}
		session.DefaultTransientStore().Delete("webui:"+id)
		_ = cleanupWebUIMedia(s.webUIMediaRoot(),id)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow","POST, DELETE");http.Error(w,"Method not allowed",405)
	}
}
