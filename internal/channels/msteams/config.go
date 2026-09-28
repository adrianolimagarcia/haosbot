package msteams

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const ChannelName = "msteams"

var defaultTrustedServiceHosts = []string{
	"smba.trafficmanager.net",
	"smba.infra.gcc.teams.microsoft.com",
	"smba.infra.gov.teams.microsoft.us",
	"smba.infra.dod.teams.microsoft.us",
	"*.botframework.com",
}

type Config struct {
	Enabled bool
	AppID string
	AppPassword string
	TenantID string
	ListenAddr string
	Path string
	AllowFrom []string
	ReplyInThread bool
	MentionOnlyResponse string
	ValidateInboundAuth bool
	TrustedServiceURLHosts []string
}

func sectionConfig(section channels.Section) (Config,error) {
	values,_:=section.Map()
	host:=strings.TrimSpace(stringValue(values["host"],"0.0.0.0"))
	port:=intValue(values["port"],3978)
	cfg:=Config{
		Enabled:boolValue(values["enabled"],false),
		AppID:strings.TrimSpace(stringValue(values["appId"],"")),
		AppPassword:stringValue(values["appPassword"],""),
		TenantID:strings.TrimSpace(stringValue(values["tenantId"],"")),
		ListenAddr:net.JoinHostPort(host,strconv.Itoa(port)),
		Path:strings.TrimSpace(stringValue(values["path"],"/api/messages")),
		AllowFrom:stringList(values["allowFrom"]),
		ReplyInThread:boolValue(values["replyInThread"],true),
		MentionOnlyResponse:stringValue(values["mentionOnlyResponse"],"Hi — what can I help with?"),
		ValidateInboundAuth:boolValue(values["validateInboundAuth"],true),
		TrustedServiceURLHosts:stringList(values["trustedServiceUrlHosts"]),
	}
	if len(cfg.TrustedServiceURLHosts)==0 { cfg.TrustedServiceURLHosts=append([]string(nil),defaultTrustedServiceHosts...) }
	if port<1 || port>65535 { return Config{},fmt.Errorf("msteams: port must be between 1 and 65535") }
	if cfg.Path=="" || !strings.HasPrefix(cfg.Path,"/") || strings.ContainsAny(cfg.Path,"?#") { return Config{},fmt.Errorf("msteams: path must be an absolute URL path") }
	for _,pattern:=range cfg.TrustedServiceURLHosts {
		p:=strings.TrimSpace(strings.ToLower(pattern))
		if p=="" || strings.ContainsAny(p,"/:?#@") { return Config{},fmt.Errorf("msteams: invalid trustedServiceUrlHosts entry %q",pattern) }
		if strings.HasPrefix(p,"*.") { p=strings.TrimPrefix(p,"*.") }
		if _,err:=url.Parse("https://"+p);err!=nil {return Config{},fmt.Errorf("msteams: invalid trusted host %q",pattern)}
	}
	return cfg,nil
}
func (c Config) validateRuntime() error {
	var missing []string
	if c.AppID=="" {missing=append(missing,"appId")}
	if c.AppPassword=="" {missing=append(missing,"appPassword")}
	if len(missing)>0{return fmt.Errorf("msteams: missing required setting(s): %s",strings.Join(missing,", "))}
	return nil
}

func stringValue(v any,f string)string{if s,ok:=v.(string);ok{return s};return f}
func boolValue(v any,f bool)bool{if b,ok:=v.(bool);ok{return b};return f}
func intValue(v any,f int)int{switch n:=v.(type){case int:return n;case int64:return int(n);case float64:return int(n)};return f}
func stringList(v any)[]string{
	switch x:=v.(type){
	case []string:return append([]string(nil),x...)
	case []any:out:=make([]string,0,len(x));for _,i:=range x{if s,ok:=i.(string);ok&&strings.TrimSpace(s)!=""{out=append(out,strings.TrimSpace(s))}};return out
	case string:var out []string;for _,i:=range strings.Split(x,","){if s:=strings.TrimSpace(i);s!=""{out=append(out,s)}};return out
	default:return nil
	}
}
