package mochat

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const ChannelName="mochat"

type Config struct{
	Enabled bool
	BaseURL string
	ClawToken string
	AgentUserID string
	Sessions []string
	Panels []string
	AllowFrom []string
	RefreshInterval time.Duration
	WatchTimeout time.Duration
	WatchLimit int
	RetryDelay time.Duration
}

func sectionConfig(section channels.Section)(Config,error){
	values,_:=section.Map()
	cfg:=Config{
		Enabled:boolValue(values["enabled"],false),
		BaseURL:strings.TrimRight(strings.TrimSpace(stringValue(values["baseUrl"],"https://mochat.io")),"/"),
		ClawToken:strings.TrimSpace(stringValue(values["clawToken"],"")),
		AgentUserID:strings.TrimSpace(stringValue(values["agentUserId"],"")),
		Sessions:stringList(values["sessions"]),Panels:stringList(values["panels"]),AllowFrom:stringList(values["allowFrom"]),
		RefreshInterval:time.Duration(intValue(values["refreshIntervalMs"],30000))*time.Millisecond,
		WatchTimeout:time.Duration(intValue(values["watchTimeoutMs"],25000))*time.Millisecond,
		WatchLimit:intValue(values["watchLimit"],100),
		RetryDelay:time.Duration(intValue(values["retryDelayMs"],500))*time.Millisecond,
	}
	u,err:=url.Parse(cfg.BaseURL);if err!=nil||u.Scheme!="https"||u.Host==""||u.User!=nil||u.RawQuery!=""||u.Fragment!=""{return Config{},fmt.Errorf("mochat: baseUrl must be an HTTPS origin")}
	if cfg.RefreshInterval<time.Second||cfg.RefreshInterval>time.Hour{return Config{},fmt.Errorf("mochat: refreshIntervalMs out of range")}
	if cfg.WatchTimeout<time.Second||cfg.WatchTimeout>2*time.Minute{return Config{},fmt.Errorf("mochat: watchTimeoutMs out of range")}
	if cfg.WatchLimit<1||cfg.WatchLimit>100{return Config{},fmt.Errorf("mochat: watchLimit must be 1..100")}
	if cfg.RetryDelay<100*time.Millisecond||cfg.RetryDelay>time.Minute{return Config{},fmt.Errorf("mochat: retryDelayMs out of range")}
	return cfg,nil
}
func(c Config)validateRuntime()error{if c.ClawToken==""{return fmt.Errorf("mochat: clawToken is required")};return nil}
func stringValue(v any,f string)string{if s,ok:=v.(string);ok{return s};return f}
func boolValue(v any,f bool)bool{if b,ok:=v.(bool);ok{return b};return f}
func intValue(v any,f int)int{switch n:=v.(type){case int:return n;case int64:return int(n);case float64:return int(n)};return f}
func stringList(v any)[]string{switch x:=v.(type){case []string:return append([]string(nil),x...);case []any:out:=make([]string,0,len(x));for _,i:=range x{if s,ok:=i.(string);ok&&strings.TrimSpace(s)!=""{out=append(out,strings.TrimSpace(s))}};return out;case string:var out []string;for _,i:=range strings.Split(x,","){if s:=strings.TrimSpace(i);s!=""{out=append(out,s)}};return out};return nil}
