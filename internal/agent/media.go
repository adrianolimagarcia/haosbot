package agent

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const (
	maxTurnMediaFiles = 8
	maxImageMediaBytes = 20 << 20
	maxTextMediaBytes  = 512 << 10
	maxPDFContextBytes = 256 << 10
)

func userMessageForModel(msg core.InboundMessage) (core.Message, error) {
	if len(msg.Media) == 0 { return *core.NewMessage(core.RoleUser, msg.Content), nil }
	if len(msg.Media) > maxTurnMediaFiles { return core.Message{}, fmt.Errorf("too many attachments: %d > %d",len(msg.Media),maxTurnMediaFiles) }
	blocks:=make([]core.ContentBlock,0,len(msg.Media)+1)
	if strings.TrimSpace(msg.Content)!="" { blocks=append(blocks,core.ContentBlock{Type:"text",Text:msg.Content}) }
	for _,rawPath:=range msg.Media {
		path:=strings.TrimSpace(rawPath); if path==""{continue}
		info,err:=os.Lstat(path); if err!=nil{return core.Message{},fmt.Errorf("attachment %s: %w",filepath.Base(path),err)}
		if !info.Mode().IsRegular()||info.Mode()&os.ModeSymlink!=0{return core.Message{},fmt.Errorf("attachment %s is not a regular file",filepath.Base(path))}
		if info.Size()>maxImageMediaBytes{return core.Message{},fmt.Errorf("attachment %s exceeds %d bytes",filepath.Base(path),maxImageMediaBytes)}
		f,err:=os.Open(path); if err!=nil{return core.Message{},err}; head:=make([]byte,512); n,_:=f.Read(head); _=f.Close(); mime:=http.DetectContentType(head[:n])
		switch mime {
		case "image/png","image/jpeg","image/gif","image/webp":
			data,err:=os.ReadFile(path); if err!=nil{return core.Message{},err}; url:="data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(data); imageURL,_:=json.Marshal(map[string]any{"url":url}); blocks=append(blocks,core.ContentBlock{Type:"image_url",ImageURL:imageURL})
		case "application/pdf":
			data,err:=os.ReadFile(path); if err!=nil{return core.Message{},err}
			if !bytes.HasPrefix(data,[]byte("%PDF-")){return core.Message{},fmt.Errorf("attachment %s has invalid PDF signature",filepath.Base(path))}
			text:=extractPDFTextBounded(data,maxPDFContextBytes)
			if text=="" { text="[PDF attachment: "+filepath.Base(path)+". No safely extractable text was found; scanned/image-only or unsupported compressed content is not injected.]" } else { text="[PDF attachment: "+filepath.Base(path)+"]\n"+text }
			blocks=append(blocks,core.ContentBlock{Type:"text",Text:text})
		default:
			if info.Size()<=maxTextMediaBytes { data,err:=os.ReadFile(path); if err!=nil{return core.Message{},err}; if utf8.Valid(data)&&(strings.HasPrefix(mime,"text/")||mime=="application/json"||mime=="application/xml"||mime=="application/octet-stream"){blocks=append(blocks,core.ContentBlock{Type:"text",Text:"[Attachment: "+filepath.Base(path)+"]\n"+string(data)});continue} }
			blocks=append(blocks,core.ContentBlock{Type:"text",Text:"[Binary attachment: "+filepath.Base(path)+" ("+mime+"). The binary contents are not injected into the model context.]"})
		}
	}
	if len(blocks)==0{return core.Message{},errors.New("attachments produced no model content")}
	return core.Message{Role:core.RoleUser,Content:core.BlockContent(blocks)},nil
}

// extractPDFTextBounded is deliberately small and dependency-free. It extracts
// literal PDF text operands from plain and FlateDecode streams, which covers
// many generated PDFs without turning PDF parsing into a hot-path dependency.
// It never executes embedded content and caps both decompression and output.
func extractPDFTextBounded(data []byte, limit int) string {
	if limit<=0{return ""}
	var out strings.Builder
	appendStream:=func(stream []byte){
		for i:=0;i<len(stream)&&out.Len()<limit;i++ { if stream[i]!='(' {continue}; i++; var b strings.Builder; depth:=1
			for i<len(stream)&&depth>0&&out.Len()+b.Len()<limit { c:=stream[i]; if c=='\\'&&i+1<len(stream){i++; switch stream[i]{case 'n':b.WriteByte('\n');case 'r':b.WriteByte('\r');case 't':b.WriteByte('\t');case '(',')','\\':b.WriteByte(stream[i]);default: if stream[i]>='0'&&stream[i]<='7'{j:=i; for j+1<len(stream)&&j<i+2&&stream[j+1]>='0'&&stream[j+1]<='7'{j++}; if v,e:=strconv.ParseInt(string(stream[i:j+1]),8,16);e==nil{b.WriteByte(byte(v))}; i=j}} } else if c=='(' {depth++; b.WriteByte(c)} else if c==')'{depth--; if depth>0{b.WriteByte(c)}} else if c>=32||c=='\n'||c=='\r'||c=='\t'{b.WriteByte(c)}; i++ }
			t:=strings.TrimSpace(b.String()); if t!=""&&utf8.ValidString(t){ if out.Len()>0{out.WriteByte('\n')}; remaining:=limit-out.Len(); if len(t)>remaining{t=t[:remaining]; for len(t)>0 && !utf8.ValidString(t){t=t[:len(t)-1]}}; out.WriteString(t) }
		}
	}
	pos:=0
	for pos<len(data)&&out.Len()<limit { idx:=bytes.Index(data[pos:],[]byte("stream")); if idx<0{break}; start:=pos+idx+6; if start<len(data)&&data[start]=='\r'{start++}; if start<len(data)&&data[start]=='\n'{start++}; endRel:=bytes.Index(data[start:],[]byte("endstream")); if endRel<0{break}; end:=start+endRel; stream:=data[start:end]; dictStart:=start-1024; if dictStart<0{dictStart=0}; dict:=data[dictStart:start]
		if bytes.Contains(dict,[]byte("/FlateDecode")){ zr,err:=zlib.NewReader(bytes.NewReader(stream)); if err==nil { decoded,_:=io.ReadAll(io.LimitReader(zr,int64(limit*4)+1)); _=zr.Close(); appendStream(decoded) } } else { appendStream(stream) }; pos=end+9 }
	return strings.TrimSpace(out.String())
}
