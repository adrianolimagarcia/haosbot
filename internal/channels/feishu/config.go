package feishu

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const ChannelName="feishu"

type Config struct{
	Enabled bool
	AppID string
	AppSecret string
	Domain string
	VerificationToken string
	SignatureKey string
	ListenAddr string
	WebhookPath string
	AllowFrom []string
}

func sectionConfig(section channels.Section)(Config,error){
	values,_:=section.Map()
	host:=strings.TrimSpace(stringValue(values["host"],"0.0.0.0"));port:=intValue(values["port"],3980)
	cfg:=Config{
		Enabled:boolValue(values["enabled"],false),AppID:strings.TrimSpace(stringValue(values["appId"],"")),AppSecret:stringValue(values["appSecret"],""),
		Domain:strings.TrimSpace(strings.ToLower(stringValue(values["domain"],"feishu"))),VerificationToken:stringValue(values["verificationToken"],""),
		SignatureKey:stringValue(values["signatureKey"],""),ListenAddr:net.JoinHostPort(host,strconv.Itoa(port)),WebhookPath:strings.TrimSpace(stringValue(values["webhookPath"],"/feishu/webhook")),
		AllowFrom:stringList(values["allowFrom"]),
	}
	if port<1||port>65535{return Config{},fmt.Errorf("feishu: port must be 1..65535")}
	if cfg.WebhookPath==""||!strings.HasPrefix(cfg.WebhookPath,"/")||strings.ContainsAny(cfg.WebhookPath,"?#"){return Config{},fmt.Errorf("feishu: webhookPath must be an absolute path")}
	if cfg.Domain!="feishu"&&cfg.Domain!="lark"{
		u,err:=url.Parse(cfg.Domain);if err!=nil||u.Scheme!="https"||u.Host==""||u.User!=nil{return Config{},fmt.Errorf("feishu: domain must be feishu, lark, or an HTTPS origin")}
		cfg.Domain=strings.TrimRight(cfg.Domain,"/")
	}
	return cfg,nil
}
func(c Config)validateRuntime()error{var m []string;if c.AppID==""{m=append(m,"appId")};if c.AppSecret==""{m=append(m,"appSecret")};if c.VerificationToken==""{m=append(m,"verificationToken")};if len(m)>0{return fmt.Errorf("feishu: missing required setting(s): %s",strings.Join(m,", "))};return nil}
func(c Config)apiBase()string{switch c.Domain{case "lark":return "https://open.larksuite.com";case "feishu","":return "https://open.feishu.cn";default:return c.Domain}}
func stringValue(v any,f string)string{if s,ok:=v.(string);ok{return s};return f}
func boolValue(v any,f bool)bool{if b,ok:=v.(bool);ok{return b};return f}
func intValue(v any,f int)int{switch n:=v.(type){case int:return n;case int64:return int(n);case float64:return int(n)};return f}
func stringList(v any)[]string{switch x:=v.(type){case []string:return append([]string(nil),x...);case []any:out:=make([]string,0,len(x));for _,i:=range x{if s,ok:=i.(string);ok&&strings.TrimSpace(s)!=""{out=append(out,strings.TrimSpace(s))}};return out;case string:var out []string;for _,i:=range strings.Split(x,","){if s:=strings.TrimSpace(i);s!=""{out=append(out,s)}};return out};return nil}
