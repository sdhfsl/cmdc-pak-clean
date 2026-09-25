package main

import (
	"context"
	"crypto/rand"
	_ "embed" // go:embed directives only
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed assets/favicon.ico
var faviconICO []byte

type Config struct {
	Token string `json:"token"`
	Port  int    `json:"port"`
	// Last known bundled CLI version. Refreshed on every successful parse
	// so a later unreadable harness (uninstalled desktop app) still sends a
	// real version instead of omitting the header (gateway 403s the omit).
	CliVersion string `json:"cli_version,omitempty"`
}

const (
	gatewayAPI   = "https://api.commandcode.ai"
	generatePath = "/alpha/generate"
	// Project context shells out to git; keep the cache long enough that the
	// request hot path rarely pays for it. Auth is a local file read.
	ctxCacheTTL  = 60 * time.Second
	authCacheTTL = 30 * time.Second
	// How often the bundled CLI version is re-read from disk (no restart
	// needed after a desktop-app update).
	cliVersionTTL = 60 * time.Second
	maxBodyBytes  = 12 << 20
)

var (
	cfg     Config
	cfgPath string
	cfgMu   sync.RWMutex
	startAt = time.Now()
	logs    []string
	logsMu  sync.Mutex
	maxLogs = 200

	sessionID   string
	projectSlug string
	workDir     string

	cliVersionMu sync.RWMutex
	// Empty until the bundled CLI's package.json is read; the header is
	// omitted while empty, matching the desktop app's behavior.
	cliVersion   string
	cliVersionAt time.Time
)

// cliVersionGet returns the bundled CLI version and re-reads it from disk at
// most once per cliVersionTTL, so desktop-app updates are picked up without
// restarting the proxy. Disk reads happen only after the TTL expires —
// including when the version could not be read, so a missing desktop app
// does not turn every request into a filesystem probe.
func cliVersionGet() string {
	cliVersionMu.RLock()
	v, at := cliVersion, cliVersionAt
	cliVersionMu.RUnlock()
	if !at.IsZero() && time.Since(at) < cliVersionTTL {
		return v
	}
	cliVersionMu.Lock()
	defer cliVersionMu.Unlock()
	// Re-check under the write lock: another goroutine may have refreshed.
	if !cliVersionAt.IsZero() && time.Since(cliVersionAt) < cliVersionTTL {
		return cliVersion
	}
	if hp := findHarnessPath(); hp != "" {
		if nv := parseCLIVersion(hp); nv != "" {
			cliVersion = nv
		}
	}
	cliVersionAt = time.Now() // also on failure: avoid hammering the disk
	if cliVersion == "" {
		// Harness unreadable: fall back to the last persisted version.
		// The gateway 403s a missing version header, so a stale real
		// version beats omitting it.
		cliVersion = configCliVersion()
	}
	return cliVersion
}

// cliVersionSet records the detected CLI version (goroutine-safe).
func cliVersionSet(v string) {
	cliVersionMu.Lock()
	defer cliVersionMu.Unlock()
	cliVersion = v
	cliVersionAt = time.Now()
	// Persist the last known good version so an unreadable harness later
	// (uninstalled desktop app) still yields a real version header. The
	// gateway 403s requests that omit it.
	if v != "" {
		cfgMu.RLock()
		cur := cfg
		cfgMu.RUnlock()
		if cur.CliVersion != v {
			cur.CliVersion = v
			_ = saveConfig(cur)
		}
	}
}

// configCliVersion returns the last persisted CLI version, "" if none.
func configCliVersion() string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return strings.TrimSpace(cfg.CliVersion)
}

var projectDirRe = regexp.MustCompile(`(?i)Primary working directory:\s*([^\r\n]+)`)

// ---- logging ----

func maskToken(t string) string {
	if t == "" {
		return ""
	}
	r := []rune(t) // rune-safe: never split a non-ASCII character
	switch {
	case len(r) <= 4:
		return string(r[:1]) + "..."
	case len(r) <= 8:
		return string(r[:2]) + "..." + string(r[len(r)-2:])
	default:
		return string(r[:4]) + "..." + string(r[len(r)-4:])
	}
}

func logLine(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	line := time.Now().Format("15:04:05") + " " + msg
	logsMu.Lock()
	logs = append(logs, line)
	if len(logs) > maxLogs {
		logs = logs[len(logs)-maxLogs:]
	}
	logsMu.Unlock()
	log.Println(line)
	if cfgPath != "" {
		dir := filepath.Dir(cfgPath)
		p := filepath.Join(dir, "cmdc-pak-clean.log")
		if st, err := os.Stat(p); err == nil && st.Size() > 5<<20 {
			_ = os.Rename(p, p+".1")
		}
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			fmt.Fprintln(f, time.Now().Format("2006-01-02 15:04:05")+" "+msg)
			f.Close()
		}
	}
}

// ---- config ----

func configDir() string {
	if d := os.Getenv("CMDC_PAK_CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("APPDATA"); d != "" {
		return filepath.Join(d, "cmdc-pak-clean")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cmdc-pak-clean")
}

func loadConfig() Config {
	dir := configDir()
	cfgPath = filepath.Join(dir, "config.json")
	_ = os.MkdirAll(dir, 0755)
	c := Config{Port: 8787}
	if b, err := os.ReadFile(cfgPath); err == nil {
		_ = json.Unmarshal(b, &c)
		if c.Port == 0 {
			c.Port = 8787
		}
	}
	if v := os.Getenv("CMDC_PAK_TOKEN"); v != "" {
		c.Token = v
	}
	if v := os.Getenv("CMDC_PAK_PORT"); v != "" {
		if p, e := strconv.Atoi(v); e == nil && p != 0 {
			c.Port = p
		}
	}
	return c
}

func saveConfig(c Config) error {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cfg = c
	_ = os.MkdirAll(filepath.Dir(cfgPath), 0755)
	b, _ := json.MarshalIndent(c, "", "  ")
	// Atomic replace so a crash mid-write cannot corrupt the config.
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, cfgPath)
}

// ---- auth ----

func authCandidates() []string {
	var out []string
	home, _ := os.UserHomeDir()
	if home != "" {
		out = append(out, filepath.Join(home, ".commandcode", "auth.json"))
		out = append(out, filepath.Join(home, ".commandcode", "auth.local.json"))
	}
	if v := os.Getenv("CMDC_PAK_AUTH_FILE"); v != "" {
		out = append([]string{v}, out...)
	}
	if up := os.Getenv("USERPROFILE"); up != "" {
		out = append(out, filepath.Join(up, ".commandcode", "auth.json"))
	}
	seen := map[string]struct{}{}
	uniq := make([]string, 0, len(out))
	for _, p := range out {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			uniq = append(uniq, p)
		}
	}
	return uniq
}

func findAuthPath() string {
	for _, p := range authCandidates() {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

type commandCodeAuth struct {
	ApiKey          string `json:"apiKey"`
	UserID          string `json:"userId"`
	UserName        string `json:"userName"`
	KeyName         string `json:"keyName"`
	AuthenticatedAt string `json:"authenticatedAt"`
}

var authCache = struct {
	sync.RWMutex
	a    *commandCodeAuth
	path string
	err  error
	exp  time.Time
}{}

func loadLocalCommandCodeAuth() (*commandCodeAuth, string, error) {
	authCache.RLock()
	if time.Now().Before(authCache.exp) {
		a, p, err := authCache.a, authCache.path, authCache.err
		authCache.RUnlock()
		if a != nil || err != nil {
			return a, p, err
		}
	} else {
		authCache.RUnlock()
	}
	path := findAuthPath()
	if path == "" {
		if tok := configToken(); tok != "" {
			return configAuth(tok), "config", nil
		}
		err := fmt.Errorf("未找到本机 Command Code 凭证，请先在桌面版登录（%s）", filepath.Join("~", ".commandcode", "auth.json"))
		authCache.Lock()
		authCache.a, authCache.path, authCache.err, authCache.exp = nil, "", err, time.Now().Add(authCacheTTL)
		authCache.Unlock()
		return nil, "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		authCache.Lock()
		authCache.a, authCache.path, authCache.err, authCache.exp = nil, path, err, time.Now().Add(authCacheTTL)
		authCache.Unlock()
		return nil, path, err
	}
	var a commandCodeAuth
	if err := json.Unmarshal(b, &a); err != nil {
		if tok := configToken(); tok != "" {
			return configAuth(tok), "config", nil
		}
		authCache.Lock()
		authCache.a, authCache.path, authCache.err, authCache.exp = nil, path, err, time.Now().Add(authCacheTTL)
		authCache.Unlock()
		return nil, path, err
	}
	if strings.TrimSpace(a.ApiKey) == "" {
		if tok := configToken(); tok != "" {
			return configAuth(tok), "config", nil
		}
		err = fmt.Errorf("auth.json 中 apiKey 为空，请重新登录")
		authCache.Lock()
		authCache.a, authCache.path, authCache.err, authCache.exp = nil, path, err, time.Now().Add(authCacheTTL)
		authCache.Unlock()
		return nil, path, err
	}
	authCache.Lock()
	authCache.a, authCache.path, authCache.err, authCache.exp = &a, path, nil, time.Now().Add(authCacheTTL)
	authCache.Unlock()
	return &a, path, nil
}

// configToken returns the manually saved token (dashboard / CMDC_PAK_TOKEN),
// used as a fallback credential when no local auth file exists.
func configToken() string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	t := strings.TrimSpace(cfg.Token)
	if t == "" {
		t = strings.TrimSpace(os.Getenv("CMDC_PAK_TOKEN"))
	}
	return t
}

// configAuth wraps a fallback token so request lanes can use it exactly like
// a file-based credential. It is deliberately NOT cached: the dashboard can
// replace the token at any time via /api/config.
func configAuth(tok string) *commandCodeAuth {
	return &commandCodeAuth{ApiKey: tok, UserName: "manual-token"}
}

// ---- upstream transport ----

var (
	sharedUpstreamOnce   sync.Once
	sharedUpstreamClient *http.Client
)

func gatewayBaseURL() string {
	if v := os.Getenv("CMDC_PAK_GATEWAY_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return gatewayAPI
}

func upstreamClient() *http.Client {
	sharedUpstreamOnce.Do(func() {
		sharedUpstreamClient = &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				Proxy:                 nil,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				MaxIdleConns:          64,
				MaxIdleConnsPerHost:   16,
				IdleConnTimeout:       90 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		}
	})
	return sharedUpstreamClient
}

// ---- model catalog (parsed from the desktop harness) ----

type modelSpec struct {
	ID            string   `json:"id"`
	ContextWindow int      `json:"context_window,omitempty"`
	Effort        []string `json:"effort,omitempty"`
}

var (
	modelMu         sync.Mutex
	parsedModels    []modelSpec
	effortSupported map[string]bool
	catalogMtime    time.Time
)

// fallbackSpecs carries the known models, context windows and thinking
// ladders used when the desktop harness cannot be read (verified against
// the live catalog), so clients still see full capability info.
var fallbackSpecs = []modelSpec{
	{ID: "deepseek/deepseek-v4.1-flash", ContextWindow: 1000000, Effort: []string{"low", "high", "max"}},
	{ID: "deepseek/deepseek-v4-flash", ContextWindow: 1000000, Effort: []string{"high", "max"}},
	{ID: "xiaomi/mimo-v2.5", ContextWindow: 200000},
	{ID: "meta/muse-spark-1.3-contributor", ContextWindow: 1048576, Effort: []string{"low", "medium", "high", "xhigh"}},
	{ID: "z-ai/glm-5.3-flash", ContextWindow: 1048576, Effort: []string{"low", "high", "max"}},
	{ID: "xai/grok-4.5", ContextWindow: 500000},
	{ID: "gpt-5.6-sol"},
	{ID: "MiniMaxAI/MiniMax-M3", ContextWindow: 1000000, Effort: []string{"low", "medium", "high"}},
}

// gatewayKnownExtras covers models the gateway serves but the local bundle
// may not have picked up yet (verified live against /alpha/generate).
var gatewayKnownExtras = []modelSpec{
	{ID: "deepseek/deepseek-v4.1-flash", ContextWindow: 1000000, Effort: []string{"low", "high", "max"}},
}

func commandCodeHarnessDirs() []string {
	var out []string
	if v := os.Getenv("CMDC_HARNESS_PATH"); v != "" {
		out = append(out, v)
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		out = append(out, filepath.Join(la, "Programs", "Command Code", "resources", "app",
			"node_modules", "@commandcode", "harness", "dist", "index.js"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, filepath.Join(home, "AppData", "Local", "Programs", "Command Code",
			"resources", "app", "node_modules", "@commandcode", "harness", "dist", "index.js"))
	}
	return out
}

func findHarnessPath() string {
	for _, p := range commandCodeHarnessDirs() {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func parseCLIVersion(harnessPath string) string {
	appDir := strings.Replace(harnessPath,
		filepath.Join("node_modules", "@commandcode", "harness", "dist", "index.js"), "", 1)
	// Current desktop layout: the bundled command-code CLI's own
	// package.json carries the version the gateway expects in
	// x-command-code-version (the app reads it the same way).
	if appDir != harnessPath {
		if b, err := os.ReadFile(filepath.Join(appDir, "node_modules", "command-code", "package.json")); err == nil {
			var pkg struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(b, &pkg) == nil {
				if v := strings.TrimSpace(pkg.Version); v != "" {
					return v
				}
			}
		}
	}
	// Legacy desktop layout kept the version in out/main/index.js.
	mainPath := strings.Replace(harnessPath,
		filepath.Join("node_modules", "@commandcode", "harness", "dist", "index.js"),
		filepath.Join("out", "main", "index.js"), 1)
	b, err := os.ReadFile(mainPath)
	if err != nil {
		return ""
	}
	if m := regexp.MustCompile(`CLI_VERSION\s*=\s*"([^"]+)"`).FindSubmatch(b); len(m) == 2 {
		return string(m[1])
	}
	return ""
}

func parseContextWindows(harness string) map[string]int {
	i := strings.Index(harness, "var KNOWN_CONTEXT_WINDOWS")
	if i < 0 {
		return nil
	}
	rest := harness[i:]
	j := strings.Index(rest, ");")
	if j < 0 {
		return nil
	}
	seg := rest[:j]
	out := map[string]int{}
	re := regexp.MustCompile(`\["([^"]+)",\s*([0-9eE.]+)\]`)
	for _, m := range re.FindAllStringSubmatch(seg, -1) {
		if f, err := strconv.ParseFloat(m[2], 64); err == nil {
			out[m[1]] = int(f)
		}
	}
	return out
}

func parseOpenSourceModels(harness string) map[string]bool {
	i := strings.Index(harness, "var MODEL_ACCESS_META = {")
	if i < 0 {
		return nil
	}
	rest := harness[i:]
	j := strings.Index(rest, "};")
	if j < 0 {
		return nil
	}
	seg := rest[:j]
	out := map[string]bool{}
	re1 := regexp.MustCompile(`"([^"]+)":\s*\{\s*provider:\s*[\w.]+,\s*category:\s*OPENSOURCE`)
	re2 := regexp.MustCompile(`"([^"]+)":\s*oss\(\)`)
	for _, m := range re1.FindAllStringSubmatch(seg, -1) {
		out[m[1]] = true
	}
	for _, m := range re2.FindAllStringSubmatch(seg, -1) {
		out[m[1]] = true
	}
	return out
}

func parseReasoningEfforts(harness string) map[string][]string {
	effortNames := map[string][]string{}
	for _, m := range regexp.MustCompile(`var (EFFORT_\w+)\s*=\s*\[([^\]]*)\]`).FindAllStringSubmatch(harness, -1) {
		var vals []string
		for _, v := range strings.Split(m[2], ",") {
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if v != "" {
				vals = append(vals, v)
			}
		}
		effortNames[m[1]] = vals
	}
	i := strings.Index(harness, "var MODEL_REASONING_EFFORTS")
	if i < 0 {
		return nil
	}
	rest := harness[i:]
	j := strings.Index(rest, ");")
	if j < 0 {
		return nil
	}
	seg := rest[:j]
	out := map[string][]string{}
	for _, m := range regexp.MustCompile(`\["([^"]+)",\s*(EFFORT_\w+)\]`).FindAllStringSubmatch(seg, -1) {
		if vals, ok := effortNames[m[2]]; ok {
			out[m[1]] = vals
		}
	}
	return out
}

func loadModelCatalogAt(hp string) ([]modelSpec, string) {
	if hp == "" {
		return nil, ""
	}
	b, err := os.ReadFile(hp)
	if err != nil {
		return nil, ""
	}
	harness := string(b)
	windows := parseContextWindows(harness)
	oss := parseOpenSourceModels(harness)
	efforts := parseReasoningEfforts(harness)

	ids := map[string]bool{}
	for id := range oss {
		ids[id] = true
	}
	for _, m := range fallbackSpecs {
		ids[m.ID] = true
	}
	extraEffort := map[string][]string{}
	extraWindow := map[string]int{}
	for _, m := range gatewayKnownExtras {
		ids[m.ID] = true
		extraWindow[m.ID] = m.ContextWindow
		extraEffort[m.ID] = m.Effort
	}
	catalog := make([]modelSpec, 0, len(ids))
	for id := range ids {
		spec := modelSpec{ID: id}
		if w, ok := windows[id]; ok {
			spec.ContextWindow = w
		} else if w, ok := extraWindow[id]; ok {
			spec.ContextWindow = w
		}
		if e, ok := efforts[id]; ok {
			spec.Effort = e
		} else if e, ok := extraEffort[id]; ok {
			spec.Effort = e
		}
		catalog = append(catalog, spec)
	}
	sort.Slice(catalog, func(a, b int) bool { return catalog[a].ID < catalog[b].ID })
	em := make(map[string]bool, len(catalog))
	for _, m := range catalog {
		if len(m.Effort) > 0 {
			em[m.ID] = true
		}
	}
	return catalog, parseCLIVersion(hp)
}

func ensureModelCatalog() {
	modelMu.Lock()
	defer modelMu.Unlock()
	hp := findHarnessPath()
	if hp == "" {
		return
	}
	st, err := os.Stat(hp)
	if err != nil {
		return
	}
	if !catalogMtime.IsZero() && st.ModTime().Equal(catalogMtime) {
		return
	}
	catalog, ver := loadModelCatalogAt(hp)
	if len(catalog) > 0 {
		if len(parsedModels) == 0 || len(catalog)*2 >= len(parsedModels) {
			parsedModels = catalog
		} else {
			logLine("model catalog parse suspiciously small (%d vs %d), keeping previous", len(catalog), len(parsedModels))
			catalogMtime = st.ModTime()
			return
		}
	}
	if ver != "" {
		cliVersionSet(ver)
	}
	effortSupported = make(map[string]bool, len(parsedModels))
	for _, m := range parsedModels {
		if len(m.Effort) > 0 {
			effortSupported[m.ID] = true
		}
	}
	catalogMtime = st.ModTime()
	if v := cliVersionGet(); v != "" {
		logLine("model catalog reloaded: %d models, cli v%s", len(parsedModels), v)
	} else {
		logLine("model catalog reloaded: %d models (cli version unknown)", len(parsedModels))
	}
}

func snapshotCatalog() ([]modelSpec, map[string]bool) {
	modelMu.Lock()
	defer modelMu.Unlock()
	cp := make([]modelSpec, len(parsedModels))
	copy(cp, parsedModels)
	em := make(map[string]bool, len(effortSupported))
	for k, v := range effortSupported {
		em[k] = v
	}
	return cp, em
}

// ---- effort ladder ----

var effortRank = map[string]int{"low": 0, "medium": 1, "high": 2, "xhigh": 3, "max": 4}

// Thinking budget guards: reasoning and text share one max_tokens quota.
// When thinking is on, the quota is raised so thinking never starves the
// text (finishReason=length with empty text). 200000 trips the upstream
// parameter bound (<=200000), so stick to 128000.
const (
	thinkingTokenFloor   = 128000
	thinkingTokenDefault = 128000
	maxTokenCap          = 128000
	// Requests below this quota are treated as auxiliary calls (title
	// generation, summaries) where forced max thinking only wastes time and
	// budget, so it stays off unless the client asks for it explicitly.
	auxTokenThreshold = 8192
)

// capMaxTokens clamps client-supplied output quotas to the safe upstream
// ceiling. Values above it (e.g. 200000 from client presets) trip the
// gateway's parameter validation.
func capMaxTokens(v int) int {
	if v > maxTokenCap {
		return maxTokenCap
	}
	return v
}

// ensureThinkingBudget raises maxTokens when thinking is enabled and the
// client quota is too small to hold both. Returns the effective value and
// whether it was raised.
func ensureThinkingBudget(maxTokens int, clientSet bool, effort string) (int, bool) {
	if effort == "" || strings.EqualFold(effort, "none") {
		return maxTokens, false
	}
	if !clientSet {
		return thinkingTokenDefault, maxTokens != thinkingTokenDefault
	}
	if maxTokens < thinkingTokenFloor {
		return thinkingTokenFloor, true
	}
	return maxTokens, false
}

func forceEffortEnabled() bool {
	return os.Getenv("CMDC_PAK_FORCE_EFFORT") != "off"
}

func maxEffortOf(model string) string {
	catalog, _ := snapshotCatalog()
	for _, m := range catalog {
		if m.ID == model && len(m.Effort) > 0 {
			best := m.Effort[0]
			for _, e := range m.Effort {
				if effortRank[e] > effortRank[best] {
					best = e
				}
			}
			return best
		}
	}
	return ""
}

func clampEffort(model, want string) string {
	catalog, supported := snapshotCatalog()
	if want == "" || !supported[model] {
		return ""
	}
	w, ok := effortRank[want]
	if !ok {
		return ""
	}
	for _, m := range catalog {
		if m.ID != model {
			continue
		}
		best, bestRank := "", -1
		minVal, minRank := "", 99
		for _, e := range m.Effort {
			r, ok := effortRank[e]
			if !ok {
				continue
			}
			if r < minRank {
				minRank, minVal = r, e
			}
			if r <= w && r > bestRank {
				bestRank, best = r, e
			}
		}
		if best != "" {
			return best
		}
		return minVal
	}
	return want
}

// ---- aliases ----

var modelAliases = map[string]string{
	"mimo-v2.5":                   "xiaomi/mimo-v2.5",
	"mimo-v2.5-pro":               "xiaomi/mimo-v2.5-pro",
	"deepseek-v4-flash":           "deepseek/deepseek-v4-flash",
	"deepseek-v4-flash-vision":    "deepseek/deepseek-v4-flash-vision-exp",
	"deepseek-v4.1-flash":         "deepseek/deepseek-v4.1-flash",
	"muse-spark-1.2-contributor":  "meta/muse-spark-1.2-contributor",
	"muse-spark-1.3-contributor":  "meta/muse-spark-1.3-contributor",
	"muse-spark-1.3":              "meta/muse-spark-1.3-contributor",
	"gemini-3.7-flash":            "google/gemini-3.7-flash",
	"gemini-3.8-flash":            "google/gemini-3.8-flash",
	"kimi-k3":                     "moonshotai/Kimi-K3",
	"qwen-3.8-max":                "Qwen/Qwen3.8-Max",
	"longcat-2.0":                 "meituan/LongCat-2.0:free",
	"minimax-m3":                  "MiniMaxAI/MiniMax-M3",
	"minimax-m3-free":             "MiniMaxAI/MiniMax-M3",
	"minimaxai/minimax-m3-free":   "MiniMaxAI/MiniMax-M3",
	"minimax-m2.7-free":           "MiniMaxAI/MiniMax-M2.7",
	"minimaxai/minimax-m2.7-free": "MiniMaxAI/MiniMax-M2.7",
	"grok-4.5":                    "xai/grok-4.5",
	"glm-5.3-flash":               "z-ai/glm-5.3-flash",
	"gpt-5.6-sol":                 "gpt-5.6-sol",
}

func resolveModel(m string) string {
	m = strings.TrimSpace(m)
	lower := strings.ToLower(m)
	if v, ok := modelAliases[lower]; ok {
		return v
	}
	// Suffix mapping is a fallback for short names only: if the full id
	// already exists in the catalog (e.g. meta/muse-spark-1.3), pass it
	// through untouched instead of rewriting it to a different model.
	if i := strings.LastIndex(lower, "/"); i >= 0 {
		if knownModel(lower) {
			return m
		}
		if v, ok := modelAliases[lower[i+1:]]; ok {
			return v
		}
	}
	return m
}

// knownModel reports whether id is a full model id present in the catalog
// or the fallback list (case-insensitive).
func knownModel(id string) bool {
	catalog, _ := snapshotCatalog()
	for _, m := range catalog {
		if strings.EqualFold(m.ID, id) {
			return true
		}
	}
	for _, m := range fallbackSpecs {
		if strings.EqualFold(m.ID, id) {
			return true
		}
	}
	return false
}

// ---- util ----

func randID(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		for i := range b {
			b[i] = chars[int(time.Now().UnixNano()+int64(i))%len(chars)]
		}
		return string(b)
	}
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000-0000-4000-8000-" + randID(12)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func toAnySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func evText(ev map[string]any) string {
	s, _ := ev["text"].(string)
	return s
}

// normalizeToolInput unwraps the malformed shapes models occasionally emit
// (null, single-element array wrapper, JSON string) into an object, mirroring
// the desktop harness coerceToolInput so clients can execute the tool.
func normalizeToolInput(v any) any {
	switch t := v.(type) {
	case nil:
		return map[string]any{}
	case []any:
		if len(t) == 1 {
			if m, ok := t[0].(map[string]any); ok {
				return m
			}
		}
		return t
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return map[string]any{}
		}
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			if m, ok := parsed.(map[string]any); ok {
				return m
			}
		}
		return t
	}
	return v
}

func toolCallArgs(ev map[string]any) string {
	if input, ok := ev["input"]; ok {
		if b, err := json.Marshal(normalizeToolInput(input)); err == nil {
			return string(b)
		}
	}
	if a, ok := ev["args"]; ok {
		if b, err := json.Marshal(normalizeToolInput(a)); err == nil {
			return string(b)
		}
	}
	// Never emit an empty string: clients parse this as JSON and would
	// fail on "".
	return "{}"
}

func eventErrorText(ev map[string]any) string {
	// The gateway emits error as a plain string in some failure modes
	// (upstream readStreamErrorEvent handles both shapes).
	if s, ok := ev["error"].(string); ok && s != "" {
		return s
	}
	if e, ok := ev["error"].(map[string]any); ok {
		if m, _ := e["message"].(string); m != "" {
			return m
		}
	}
	if m, _ := ev["message"].(string); m != "" {
		return m
	}
	return ""
}

func upstreamMessage(body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return string(body)
}

func emptySchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func proxyError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"type":"proxy_error","code":%d}}`, msg, status)
}

// ---- server context (project dir + git) ----

var ignoreDirs = map[string]bool{
	"node_modules": true, "dist": true, "build": true, ".git": true, ".svn": true,
	".hg": true, "coverage": true, ".nyc_output": true, ".cache": true,
	"tmp": true, "temp": true, ".next": true, ".nuxt": true, "out": true,
}

type ctxCacheEntry struct {
	v   map[string]any
	exp time.Time
}

var ctxCache = struct {
	sync.RWMutex
	m map[string]ctxCacheEntry
}{m: make(map[string]ctxCacheEntry)}

// Context payload guards: the project snapshot travels in every upstream
// request, so a huge repo (many files / dirty tree) would otherwise put
// megabytes of noise into the model's context. Both limits are far above
// anything the model needs.
const (
	maxCtxOutputRunes = 8000
	maxStructureItems = 500
)

func shellOutput(dir, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if r := []rune(s); len(r) > maxCtxOutputRunes {
		s = string(r[:maxCtxOutputRunes]) + "\n…(truncated)"
	}
	return s
}

func readStructure(dir string) []string {
	roots := []string{}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, ".") || ignoreDirs[n] {
				continue
			}
			roots = append(roots, n)
		}
		sort.Strings(roots)
		if len(roots) > maxStructureItems {
			roots = append(roots[:maxStructureItems], "…(truncated)")
		}
	}
	return roots
}

func cloneContext(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cacheContext(dir string, ctx map[string]any) {
	ctxCache.Lock()
	defer ctxCache.Unlock()
	if len(ctxCache.m) >= 64 {
		now := time.Now()
		for k, e := range ctxCache.m {
			if now.After(e.exp) {
				delete(ctxCache.m, k)
			}
		}
		if len(ctxCache.m) >= 64 {
			ctxCache.m = make(map[string]ctxCacheEntry)
		}
	}
	ctxCache.m[dir] = ctxCacheEntry{v: ctx, exp: time.Now().Add(ctxCacheTTL)}
}

func buildServerContext(dir string) map[string]any {
	ctxCache.RLock()
	if e, ok := ctxCache.m[dir]; ok && time.Now().Before(e.exp) {
		ctxCache.RUnlock()
		return cloneContext(e.v)
	}
	ctxCache.RUnlock()

	ctx := map[string]any{
		"workingDir":    dir,
		"date":          time.Now().Format("2006-01-02"),
		"environment":   "win32",
		"structure":     readStructure(dir),
		"isGitRepo":     false,
		"currentBranch": "",
		"mainBranch":    "",
		"gitStatus":     "",
		"recentCommits": []string{},
	}
	if shellOutput(dir, "git", "rev-parse", "--git-dir") == "" {
		cacheContext(dir, ctx)
		return ctx
	}
	ctx["isGitRepo"] = true
	ctx["currentBranch"] = shellOutput(dir, "git", "branch", "--show-current")
	mainBranch := ""
	if ref := shellOutput(dir, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); ref != "" {
		mainBranch = strings.TrimPrefix(ref, "origin/")
	} else if remotes := shellOutput(dir, "git", "branch", "-r"); remotes != "" {
		if strings.Contains(remotes, "origin/main") {
			mainBranch = "main"
		} else if strings.Contains(remotes, "origin/master") {
			mainBranch = "master"
		}
	}
	if mainBranch == "" {
		mainBranch = "main"
	}
	ctx["mainBranch"] = mainBranch
	status := shellOutput(dir, "git", "status", "--porcelain")
	if status == "" {
		status = "Working tree clean"
	}
	ctx["gitStatus"] = status
	commits := shellOutput(dir, "git", "log", "--oneline", "-3")
	var arr []string
	if commits != "" {
		arr = strings.Split(commits, "\n")
	}
	ctx["recentCommits"] = arr
	cacheContext(dir, ctx)
	return ctx
}

func projectDirFromMessages(msgs []any) string {
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		var candidates []string
		switch c := m["content"].(type) {
		case string:
			candidates = append(candidates, c)
		case []any:
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok {
					if t, _ := pm["type"].(string); t == "text" {
						if s, _ := pm["text"].(string); s != "" {
							candidates = append(candidates, s)
						}
					}
				}
			}
		}
		for _, s := range candidates {
			if m := projectDirRe.FindStringSubmatch(s); len(m) == 2 {
				dir := strings.TrimSpace(m[1])
				if st, err := os.Stat(dir); err == nil && st.IsDir() {
					return dir
				}
			}
		}
	}
	return ""
}

func ctxDir(msgs []any) string {
	if p := projectDirFromMessages(msgs); p != "" {
		return p
	}
	return workDir
}
