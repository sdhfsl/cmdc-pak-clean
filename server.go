package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---- usage / billing ----

type usageWindow struct {
	Used     float64 `json:"used"`
	Cap      float64 `json:"cap"`
	Exceeded bool    `json:"exceeded"`
	ResetAt  int64   `json:"resetAt"`
}

type usageInfo struct {
	PlanID          string       `json:"plan_id,omitempty"`
	MonthlyCredits  float64      `json:"monthly_credits"`
	PurchasedCredit float64      `json:"purchased_credits"`
	FreeCredits     float64      `json:"free_credits"`
	Weekly          *usageWindow `json:"weekly,omitempty"`
	FiveHour        *usageWindow `json:"five_hour,omitempty"`
	PeriodEnd       string       `json:"period_end,omitempty"`
	FetchedAt       time.Time    `json:"fetched_at"`
	Error           string       `json:"error,omitempty"`
}

var (
	usageCache      *usageInfo
	usageCacheMu    sync.Mutex
	usageCacheAt    time.Time
	usageRefreshing bool
)

// Billing cache TTLs: healthy data is served 5 minutes, errors only 1 so a
// transient upstream blip doesn't stick on the dashboard.
const (
	usageOKTTL  = 5 * time.Minute
	usageErrTTL = 1 * time.Minute
)

// fetchUsage never blocks a request on the network: it serves the cache and
// refreshes stale data in the background (single-flight).
func fetchUsage() *usageInfo {
	usageCacheMu.Lock()
	cached, at, refreshing := usageCache, usageCacheAt, usageRefreshing
	usageCacheMu.Unlock()
	ttl := usageOKTTL
	if cached != nil && cached.Error != "" {
		ttl = usageErrTTL
	}
	if cached != nil && time.Since(at) < ttl {
		return cached
	}
	if !refreshing {
		usageCacheMu.Lock()
		usageRefreshing = true
		usageCacheMu.Unlock()
		go func() {
			u := loadUsage()
			usageCacheMu.Lock()
			usageCache, usageCacheAt, usageRefreshing = u, time.Now(), false
			usageCacheMu.Unlock()
		}()
	}
	if cached != nil {
		return cached
	}
	return nil
}

func loadUsage() *usageInfo {
	u := &usageInfo{FetchedAt: time.Now()}
	auth, _, err := loadLocalCommandCodeAuth()
	if err != nil {
		u.Error = "no local auth"
		return u
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	getJSON := func(ep string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatewayBaseURL()+ep, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+auth.ApiKey)
		req.Header.Set("User-Agent", "cli")
		if v := cliVersionGet(); v != "" {
			req.Header.Set("x-command-code-version", v)
		}
		req.Header.Set("x-cli-environment", "production")
		req.Header.Set("x-project-slug", projectSlug)
		resp, err := upstreamClient().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s %d", ep, resp.StatusCode)
		}
		return json.Unmarshal(b, out)
	}
	var credits struct {
		Credits struct {
			MonthlyCredits  float64 `json:"monthlyCredits"`
			PurchasedCredit float64 `json:"purchasedCredits"`
			FreeCredits     float64 `json:"freeCredits"`
		} `json:"credits"`
		WindowLimits struct {
			FiveHour *usageWindow `json:"fiveHour"`
			Weekly   *usageWindow `json:"weekly"`
		} `json:"windowLimits"`
	}
	if err := getJSON("/alpha/billing/credits", &credits); err != nil {
		u.Error = err.Error()
	} else {
		u.MonthlyCredits = credits.Credits.MonthlyCredits
		u.PurchasedCredit = credits.Credits.PurchasedCredit
		u.FreeCredits = credits.Credits.FreeCredits
		u.FiveHour = credits.WindowLimits.FiveHour
		u.Weekly = credits.WindowLimits.Weekly
	}
	// Response shape: {"success":true,"data":{"planId":"...","currentPeriodEnd":"..."}}
	var sub struct {
		Data struct {
			PlanID           string `json:"planId"`
			CurrentPeriodEnd string `json:"currentPeriodEnd"`
		} `json:"data"`
	}
	if err := getJSON("/alpha/billing/subscriptions", &sub); err == nil {
		u.PlanID = sub.Data.PlanID
		u.PeriodEnd = sub.Data.CurrentPeriodEnd
	}
	return u
}

// ---- status / config ----

type statusResp struct {
	App             string      `json:"app"`
	Status          string      `json:"status"`
	Port            int         `json:"port"`
	UptimeSeconds   int64       `json:"uptime_seconds"`
	TokenMasked     string      `json:"token_masked"`
	Offline         bool        `json:"offline"`
	Logs            []string    `json:"logs"`
	AuthPath        string      `json:"auth_path"`
	AuthUser        string      `json:"auth_user"`
	CliVersion      string      `json:"cli_version"`
	Models          []modelSpec `json:"models,omitempty"`
	Usage           *usageInfo  `json:"usage,omitempty"`
	UpdateAvailable bool        `json:"update_available"`
}

// isSelfInstance verifies the listener on port is our own dashboard.
func isSelfInstance(port int) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var st struct {
		App string `json:"app"`
	}
	if json.NewDecoder(resp.Body).Decode(&st) != nil {
		return false
	}
	return st.App == "cmdc-pak-clean"
}

// fallbackCatalog returns the built-in model list used when the desktop
// harness cannot be read. Shared by /api/status and /v1/models.
func fallbackCatalog() []modelSpec {
	models := make([]modelSpec, 0, len(fallbackModels))
	for _, id := range fallbackModels {
		models = append(models, modelSpec{ID: id})
	}
	return models
}

// isLoopbackHost reports whether the request Host is a loopback address.
// The dashboard only binds 127.0.0.1, but an explicit check keeps /api/config
// (port rewrite + process restart) out of reach from non-local origins.
func isLoopbackHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.ToLower(strings.Trim(h, "[]"))
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// isTrustedOrigin blocks CSRF from web pages: browsers send Origin for
// cross-site requests, while the dashboard itself is same-origin and
// non-browser clients (curl, SDKs) send no Origin at all.
func isTrustedOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return isLoopbackHost(u.Host)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	ensureModelCatalog()
	cfgMu.RLock()
	c := cfg
	cfgMu.RUnlock()
	logsMu.Lock()
	cp := append([]string(nil), logs...)
	logsMu.Unlock()
	auth, authPath, _ := loadLocalCommandCodeAuth()
	masked := ""
	user := ""
	status := "no-auth"
	if auth != nil {
		masked = maskToken(auth.ApiKey)
		user = auth.UserName
		if user == "" {
			user = auth.UserID
		}
		status = "running"
	} else {
		masked = maskToken(c.Token)
	}
	w.Header().Set("Content-Type", "application/json")
	statusModels, _ := snapshotCatalog()
	if len(statusModels) == 0 {
		statusModels = fallbackCatalog()
	}
	_ = json.NewEncoder(w).Encode(statusResp{
		App:             "cmdc-pak-clean",
		Status:          status,
		Port:            c.Port,
		UptimeSeconds:   int64(time.Since(startAt).Seconds()),
		TokenMasked:     masked,
		Offline:         true,
		Logs:            cp,
		AuthPath:        authPath,
		AuthUser:        user,
		CliVersion:      cliVersionGet(),
		Models:          statusModels,
		Usage:           fetchUsage(),
		UpdateAvailable: false,
	})
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackHost(r.Host) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !isTrustedOrigin(r) {
		logLine("POST /api/config from cross-origin page blocked (origin=%s)", r.Header.Get("Origin"))
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		Token *string `json:"token"`
		Port  *int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	cfgMu.RLock()
	cur := cfg
	oldPort := cur.Port
	cfgMu.RUnlock()
	if req.Token != nil {
		if t := strings.TrimSpace(*req.Token); t != "" {
			cur.Token = t
		}
	}
	if req.Port != nil && *req.Port != 0 {
		if *req.Port < 1 || *req.Port > 65535 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": "port must be 1-65535"})
			return
		}
		cur.Port = *req.Port
	}
	if err := saveConfig(cur); err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": err.Error()})
		return
	}
	logLine("POST /api/config -> 200 (token %s, port %d)", maskToken(cur.Token), cur.Port)
	needRestart := req.Port != nil && *req.Port != 0 && *req.Port != oldPort
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "port": cur.Port, "needRestart": needRestart})
	if needRestart {
		go restartProcess(cur.Port)
	}
}

func restartProcess(port int) {
	exe, err := os.Executable()
	if err != nil {
		logLine("restart: cannot resolve executable: %v", err)
		return
	}
	if _, pinned := os.LookupEnv("CMDC_PAK_PORT"); pinned {
		logLine("restart: CMDC_PAK_PORT is set in the environment; dashboard port changes won't stick until it is unset")
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), fmt.Sprintf("CMDC_PAK_PORT=%d", port))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		logLine("restart: failed to spawn: %v", err)
		return
	}
	logLine("restart: spawned pid=%d on port %d, waiting for readiness", cmd.Process.Pid, port)
	go func() {
		for i := 0; i < 60; i++ {
			time.Sleep(300 * time.Millisecond)
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", port))
			if err == nil {
				var st struct {
					Status        string `json:"status"`
					UptimeSeconds int64  `json:"uptime_seconds"`
				}
				if json.NewDecoder(resp.Body).Decode(&st) == nil && st.UptimeSeconds < 30 {
					resp.Body.Close()
					logLine("restart: new instance healthy on %d, exiting", port)
					os.Exit(0)
				}
				resp.Body.Close()
			}
		}
		logLine("restart: new instance did not become healthy, keeping current process")
	}()
}

// ---- models endpoint ----

func handleModels(w http.ResponseWriter, r *http.Request) {
	ensureModelCatalog()
	models, _ := snapshotCatalog()
	if len(models) == 0 {
		models = fallbackCatalog()
	}
	data := make([]any, 0, len(models))
	for _, m := range models {
		// created is part of the OpenAI Model object; include it for clients
		// that validate against the spec.
		entry := map[string]any{"id": m.ID, "object": "model", "created": 0, "owned_by": "commandcode"}
		if m.ContextWindow > 0 {
			entry["context_window"] = m.ContextWindow
		}
		if len(m.Effort) > 0 {
			entry["effort"] = m.Effort
		}
		data = append(data, entry)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	logLine("GET %s -> 200 (%d models)", r.URL.Path, len(data))
}

// ---- dashboard ----

const dashboardHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>cmdc-pak-clean · Command Code Go 转换器（无校验版）</title>
<link rel="icon" href="/favicon.ico">
<style>
:root{--green:#16a34a;--green-dark:#15803d;--green-soft:#f0fdf4;--border:#e5e7eb;--bg:#eef3f1;--card:#ffffff;--text:#1f2937;--sub:#6b7280;--warn:#d97706;--red:#dc2626;--mono:ui-monospace,SFMono-Regular,Consolas,monospace}
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,"Segoe UI","Microsoft YaHei",sans-serif;background:radial-gradient(1100px 380px at 50% -80px,#d3f4e0 0%,var(--bg) 60%);color:var(--text);display:flex;justify-content:center;padding:30px 16px 64px;min-height:100vh}
.page{width:100%;max-width:980px}
.topbar{display:flex;justify-content:space-between;align-items:center;gap:12px;flex-wrap:wrap;background:var(--card);border:1px solid var(--border);border-radius:16px;padding:15px 20px;margin-bottom:16px;box-shadow:0 1px 3px rgba(0,0,0,.05)}
.brand{display:flex;gap:12px;align-items:center}
.logo{width:40px;height:40px;border-radius:12px;background:linear-gradient(135deg,var(--green),var(--green-dark));display:flex;align-items:center;justify-content:center;color:#fff;font-weight:700;font-size:19px;flex-shrink:0}
.brand h1{font-size:17px;font-weight:650}
.brand small{display:block;color:var(--sub);font-size:12px;margin-top:2px}
.top-right{display:flex;align-items:center;gap:10px;flex-wrap:wrap}
.pill{display:inline-flex;align-items:center;gap:7px;font-size:12px;font-weight:600;border-radius:999px;padding:5px 13px;border:1px solid var(--border);background:#f9fafb;color:var(--sub)}
.pill .dot{width:8px;height:8px;border-radius:50%;background:#9ca3af}
.pill.ok{background:var(--green-soft);border-color:#bbf7d0;color:var(--green-dark)}
.pill.ok .dot{background:var(--green);animation:pulse 1.6s infinite}
.pill.bad{background:#fef2f2;border-color:#fecaca;color:var(--red)}
.pill.bad .dot{background:var(--red)}
@keyframes pulse{0%{box-shadow:0 0 0 0 rgba(22,163,74,.45)}70%{box-shadow:0 0 0 7px rgba(22,163,74,0)}100%{box-shadow:0 0 0 0 rgba(22,163,74,0)}}
.meta{font-size:12px;color:var(--sub)}
.meta b{color:var(--text);font-family:var(--mono)}
.grid{display:grid;grid-template-columns:1fr 1fr;gap:16px;margin-bottom:16px}
@media(max-width:860px){.grid{grid-template-columns:1fr}}
.card{background:var(--card);border:1px solid var(--border);border-radius:16px;padding:20px 20px 18px;margin-bottom:16px;box-shadow:0 1px 3px rgba(0,0,0,.05)}
.card h2{font-size:14px;font-weight:650;margin-bottom:6px;display:flex;align-items:center;gap:8px}
.card h2 .count{font-size:11px;font-weight:500;color:var(--sub);background:#f3f4f6;border-radius:999px;padding:1px 9px}
.step{display:inline-flex;align-items:center;justify-content:center;width:20px;height:20px;border-radius:50%;background:var(--green);color:#fff;font-size:11px;font-weight:700;flex-shrink:0}
.field{margin:13px 0}.field:last-child{margin-bottom:0}
.field label{font-size:13px;font-weight:600;display:flex;align-items:center;gap:8px;margin-bottom:7px}
.kv{font-family:var(--mono);background:#f9fafb;border:1px solid var(--border);border-radius:10px;padding:10px 12px;font-size:13px;word-break:break-all;display:flex;justify-content:space-between;gap:12px;align-items:center}
.hint{font-size:12px;color:var(--sub);margin-top:6px;line-height:1.5}
.btn{border:1px solid var(--border);background:#fff;border-radius:8px;padding:6px 12px;font-size:12px;cursor:pointer;flex-shrink:0;transition:all .15s}
.btn:hover{border-color:var(--green);color:var(--green-dark)}
.btn:disabled{opacity:.55;cursor:default}
.btn-primary{background:var(--green);border-color:var(--green);color:#fff;font-weight:600}
.btn-primary:hover{background:var(--green-dark);color:#fff}
.proto{display:flex;gap:8px;flex-wrap:wrap}
.proto code{background:var(--green-soft);border:1px solid #bbf7d0;color:var(--green-dark);padding:4px 10px;border-radius:8px;font-size:12px;font-family:var(--mono)}
.stat{display:flex;justify-content:space-between;align-items:center;gap:10px;padding:9px 0;border-bottom:1px dashed #eef0f2;font-size:13px}
.stat:last-child{border-bottom:none}
.stat .k{color:var(--sub);flex-shrink:0}
.stat .v{font-weight:600;text-align:right}
.search{width:100%;padding:8px 12px;border:1px solid var(--border);border-radius:10px;font-size:13px;margin:8px 0 4px;outline:none}
.search:focus{border-color:var(--green);box-shadow:0 0 0 3px rgba(22,163,74,.12)}
.tablewrap{max-height:430px;overflow:auto}
.models{width:100%;border-collapse:collapse;font-size:12px}
.models th{color:var(--sub);font-weight:500;text-align:left;border-bottom:1px solid var(--border);padding:8px 6px;position:sticky;top:0;background:var(--card)}
.models td{border-bottom:1px solid #f3f4f6;padding:9px 6px;vertical-align:top}
.models tbody tr{cursor:pointer}
.models tbody tr:hover td{background:#f9fafb}
.models code{background:#f3f4f6;padding:2px 6px;border-radius:6px;font-size:12px;font-family:var(--mono)}
.log{font-family:var(--mono);font-size:11px;background:#0b1220;color:#cbd5e1;border-radius:10px;padding:12px;max-height:260px;overflow:auto;line-height:1.6}
.warn{color:var(--warn);font-size:11px}
.toast{position:fixed;left:50%;bottom:28px;transform:translateX(-50%) translateY(20px);background:#111827;color:#fff;font-size:13px;padding:9px 18px;border-radius:10px;opacity:0;transition:all .25s;pointer-events:none;z-index:99}
.toast.show{opacity:1;transform:translateX(-50%) translateY(0)}
</style>
</head>
<body>
<div class="page">
  <div class="topbar">
    <div class="brand"><div class="logo">C</div><div><h1>cmdc-pak-clean</h1><small>自动读取本机账号 · 直连官方网关 · 端口 <span id="portTitle">8787</span></small></div></div>
    <div class="top-right">
      <span class="pill" id="statusPill"><span class="dot"></span><span id="statusText">加载中…</span></span>
      <span class="meta">已运行 <b id="uptime">—</b></span>
    </div>
  </div>

  <div class="grid">
    <div class="card">
      <h2><span class="step">1</span>接入配置</h2>
      <div class="field"><label>Base URL</label><div class="kv"><span id="baseurl">http://localhost:8787/v1</span><button class="btn" id="btn-copy">复制</button></div></div>
      <div class="field"><label>API Key <span style="font-weight:400;color:var(--sub)">（随便填，不能留空）</span></label><div class="kv"><span>local-proxy</span><button class="btn" id="btn-copy-key">复制</button></div></div>
      <div class="field"><label>API 格式（三选一）</label><div class="proto"><code>Chat Completions</code><code>Anthropic · 思考链可见</code><code>Responses · Codex</code></div></div>
    </div>

    <div class="card">
      <h2><span class="step">2</span>账号与额度</h2>
      <div class="stat"><span class="k">本地账号</span><span class="v"><span id="authUser">—</span> <span id="masked" style="color:var(--sub);font-weight:400"></span></span></div>
      <div class="stat"><span class="k">凭证状态</span><span class="v" id="authState">—</span></div>
      <div class="stat"><span class="k">余额</span><span class="v">💰 <span id="uBalance">—</span> <span id="uDetail" style="color:var(--sub);font-weight:400;font-size:12px"></span></span></div>
      <div class="stat"><span class="k">窗口用量</span><span class="v" id="uWindows" style="font-weight:400;font-size:12px">—</span></div>
      <div class="warn" id="uErr" style="margin-top:6px"></div>
    </div>
  </div>

  <div class="card">
    <h2><span class="step">3</span>可用模型 <span class="count" id="modelCount"></span></h2>
    <input class="search" id="modelSearch" type="text" placeholder="搜索模型名…（点击行复制模型名）">
    <div class="tablewrap"><table class="models">
      <tr><th>模型名（Go 计划可用）</th><th>上下文</th><th>思考强度</th></tr>
      <tbody id="modelRows"><tr><td colspan="3" style="color:var(--sub)">加载中…</td></tr></tbody>
    </table></div>
  </div>

  <div class="card">
    <h2>请求日志 <span class="count">近 200 条</span></h2>
    <div class="log" id="logbox">暂无记录</div>
  </div>

  <div class="card">
    <h2>监听端口 <span class="count">保存后自动重启</span></h2>
    <div class="field"><div class="kv"><input id="port3" type="number" style="width:110px;padding:6px 8px;border:1px solid var(--border);border-radius:8px;font-size:13px"><button class="btn btn-primary" id="btn-port" style="margin-left:8px">保存并重启</button></div>
    <div class="hint" id="portMsg" style="margin-top:6px"></div></div>
  </div>
</div>
<div class="toast" id="toast"></div>
<script>
const $=id=>document.getElementById(id);
let allModels=[];
function toast(msg){const t=$('toast');t.textContent=msg;t.classList.add('show');clearTimeout(t._h);t._h=setTimeout(()=>t.classList.remove('show'),1400);}
function copyText(txt,msg){navigator.clipboard.writeText(txt).then(()=>toast(msg||'已复制'));}
function fmt(s){if(s<60)return s+' 秒';if(s<3600)return Math.floor(s/60)+' 分钟';return Math.floor(s/3600)+' 小时 '+Math.floor(s%3600/60)+' 分';}
function fmtWin(n){if(n>=1048576)return (n/1048576).toFixed(n%1048576?1:0)+'M';if(n>=1024)return (n/1024).toFixed(0)+'K';return n;}
function planName(id){const m={'individual-go':'Go','individual-goat':'GOAT','individual-pro':'Pro','individual-pro-v1':'Pro','individual-provider':'Provider','individual-max':'Max','individual-ultra':'Ultra','teams-pro':'Teams Pro'};return m[id]||(id||'当前套餐');}
function planQuota(id){const m={'individual-go':10,'individual-goat':70,'individual-pro':30,'individual-pro-v1':80,'individual-provider':15,'individual-max':150,'individual-ultra':300,'teams-pro':40};return (id in m)?m[id]:null;}
function parsePe(v){
  if(!v) return null;
  if(/^\d{4}-\d{2}-\d{2}/.test(v)) return new Date(v.replace(' ','T'));
  const t=new Date(v);
  return isNaN(t)?null:t;
}
function esc(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}
function renderModels(){
  const q=($('modelSearch').value||'').trim().toLowerCase();
  const rows=$('modelRows');
  const list=q?allModels.filter(m=>(m.id||'').toLowerCase().includes(q)):allModels;
  if(!list.length){rows.innerHTML='<tr><td colspan="3" style="color:var(--sub)">无匹配模型</td></tr>';return;}
  const pos=($('modelSearch').selectionStart||0);
  rows.innerHTML=list.map(m=>'<tr data-id="'+esc(m.id)+'"><td><code>'+esc(m.id)+'</code></td><td>'+(m.context_window?fmtWin(m.context_window):'—')+'</td><td>'+(m.effort&&m.effort.length?m.effort.join(' / '):'—')+'</td></tr>').join('');
  rows.querySelectorAll('tr[data-id]').forEach(tr=>tr.onclick=()=>copyText(tr.dataset.id,'模型名已复制：'+tr.dataset.id));
  if(document.activeElement===$('modelSearch')) $('modelSearch').setSelectionRange(pos,pos);
}
$('btn-copy').onclick=()=>copyText($('baseurl').textContent);
$('btn-copy-key').onclick=()=>copyText('local-proxy','API Key 已复制：local-proxy');
$('modelSearch').oninput=renderModels;
$('btn-port').onclick=async()=>{
  const p=parseInt($('port3').value,10); const msg=$('portMsg');
  if(!p||p<1||p>65535){msg.textContent='❌ 端口无效';return;}
  msg.textContent='保存中…'; const b=$('btn-port'); b.disabled=true;
  try{
    const r=await fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({port:p})});
    const res=await r.json();
    if(res.needRestart){msg.textContent='✓ 已保存，服务自动重启中…';setTimeout(()=>location.href='http://localhost:'+res.port+'/',2500);}
    else if(!res.ok){msg.textContent='❌ '+res.message;}
    else{msg.textContent='✓ 已保存';}
  }catch(e){msg.textContent='❌ 无法连接本地服务';}
  b.disabled=false;
};
async function poll(){
  try{
    const r=await fetch('/api/status'); const v=await r.json();
    const pill=$('statusPill'),st=$('statusText');
    if(v.status==='running'){pill.className='pill ok';st.textContent='运行中';}
    else{pill.className='pill bad';st.textContent=v.status||'异常';}
    $('masked').textContent=v.token_masked? ' · '+v.token_masked:'';
    $('authUser').textContent=v.auth_user || '（未找到）';
    $('authState').textContent=v.status==='running'?'已读取本地凭证':'缺少本地凭证';
    $('authState').style.color=v.status==='running'?'#16a34a':'#dc2626';
    $('portTitle').textContent=v.port;
    $('baseurl').textContent='http://localhost:'+v.port+'/v1';
    if(document.activeElement!==$('port3')) $('port3').value=v.port;
    $('uptime').textContent=fmt(v.uptime_seconds);
    if(v.usage){
      if(v.usage.error){ $('uBalance').textContent='—'; $('uErr').textContent='('+v.usage.error+')'; }
      else{
        $('uErr').textContent='';
        const num=x=>(typeof x==='number'&&isFinite(x))?x:0;
        const bal=num(v.usage.monthly_credits)+num(v.usage.purchased_credits)+num(v.usage.free_credits);
        $('uBalance').textContent='$'+bal.toFixed(2);
        const pe=parsePe(v.usage.period_end);
        const days=(pe&&!isNaN(pe))? Math.max(0,Math.ceil((pe-Date.now())/86400000)) : null;
        const quota=planQuota(v.usage.plan_id);
        const quotaTxt=quota===null?'（额度未知）':'月额度 $'+quota;
        $('uDetail').textContent='（'+planName(v.usage.plan_id)+' '+quotaTxt+(days===null?'':' · '+days+' 天后重置')+'）';
        const fmtW=(w,label)=>w? label+' $'+num(w.used).toFixed(2)+'/$'+(w.cap||'—')+(w.exceeded?' ❌':'') : '';
        $('uWindows').textContent='窗口用量：'+[fmtW(v.usage.five_hour,'5小时'),fmtW(v.usage.weekly,'本周')].filter(Boolean).join(' · ');
      }
    }
    const rows=$('modelRows');
    if(v.models&&v.models.length){
      allModels=v.models;
      $('modelCount').textContent=v.models.length+' 个';
      const sig=v.models.map(m=>m.id+':'+(m.context_window||'')+':'+((m.effort||[]).join(','))).join('|');
      if(sig!==window._modelSig){window._modelSig=sig;renderModels();}
    } else {
      rows.innerHTML='<tr><td colspan="3" style="color:var(--warn)">未读取到模型目录（已用内置回退列表）</td></tr>';
    }
    const logs=v.logs||[];
    const logSig=logs.length+'|'+(logs[logs.length-1]||'');
    const lb=$('logbox');
    if(lb.dataset.sig!==logSig){
      lb.dataset.sig=logSig;
      const prevTop=lb.scrollTop;
      lb.innerHTML=logs.length?logs.slice().reverse().map(l=>'<div>'+l.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')+'</div>').join(''):'<div>暂无请求记录</div>';
      lb.scrollTop=prevTop;
    }
  }catch(e){}
}
poll(); setInterval(poll,5000);
</script>
</body>
</html>`

func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, dashboardHTML)
}

// ---- main ----

func main() {
	cfg = loadConfig()
	log.SetFlags(0)

	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port), 400*time.Millisecond); err == nil {
		conn.Close()
		if isSelfInstance(cfg.Port) {
			log.Printf("already running on %d, exiting", cfg.Port)
			return
		}
		log.Printf("port %d occupied by another program, trying next ports", cfg.Port)
	}

	sessionID = "sess-" + randID(16)
	if v := os.Getenv("CMDC_PAK_PROJECT_DIR"); v != "" {
		workDir = v
	} else if wd, err := os.Getwd(); err == nil {
		workDir = wd
	}
	projectSlug = filepath.Base(workDir)
	ensureModelCatalog()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/v1/chat/completions", handleChat)
	mux.HandleFunc("/v1/messages", handleMessages)
	mux.HandleFunc("/messages", handleMessages)
	mux.HandleFunc("/v1/responses", handleResponses)
	mux.HandleFunc("/responses", handleResponses)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/v1/chat", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(faviconICO)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	auth, authPath, _ := loadLocalCommandCodeAuth()
	if auth != nil {
		logLine("cmdc-pak-clean starting on %s (auto-read %s user=%s %s)", addr, authPath, auth.UserName, maskToken(auth.ApiKey))
	} else {
		logLine("cmdc-pak-clean starting on %s (no local auth found)", addr)
	}
	models, _ := snapshotCatalog()
	if len(models) > 0 {
		if v := cliVersionGet(); v != "" {
			logLine("model catalog: %d models, cli v%s", len(models), v)
		} else {
			logLine("model catalog: %d models (cli version unknown)", len(models))
		}
	} else {
		logLine("model catalog: using %d fallback models", len(fallbackModels))
	}
	logLine("upstream %s%s", gatewayBaseURL(), generatePath)
	if toolNudgeEnabled() {
		logLine("tool nudge: ON (appends tool-usage instruction to system prompt when tools are declared)")
	}

	var lastErr error
	for i := 0; i < 5; i++ {
		// No WriteTimeout: SSE responses can legitimately stream for
		// minutes. ReadHeaderTimeout bounds slow-header clients.
		srv := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		lastErr = srv.ListenAndServe()
		if lastErr != nil && strings.Contains(strings.ToLower(lastErr.Error()), "bind") {
			cfg.Port++
			addr = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
			logLine("port occupied, trying %s (not saved to config; restart keeps original port %d)", addr, cfg.Port-1)
			continue
		}
		log.Fatalf("listen: %v", lastErr)
	}
	log.Fatalf("failed to bind after retries: %v", lastErr)
}
