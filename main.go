package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "0.3.0"

type Tool struct {
	Slug  string `json:"slug"`
	Label string `json:"label"`
	Desc  string `json:"desc"`
	Port  int    `json:"port"`
}

type Bundle struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Headline string `json:"headline"`
	Tools    []Tool `json:"tools"`
}

// Custom unmarshal to handle tools as either strings or objects
func (b *Bundle) UnmarshalJSON(data []byte) error {
	type Alias struct {
		Slug     string            `json:"slug"`
		Name     string            `json:"name"`
		Headline string            `json:"headline"`
		Tools    []json.RawMessage `json:"tools"`
	}
	var a Alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	b.Slug = a.Slug
	b.Name = a.Name
	b.Headline = a.Headline
	for _, raw := range a.Tools {
		var t Tool
		if err := json.Unmarshal(raw, &t); err == nil && t.Slug != "" {
			b.Tools = append(b.Tools, t)
		} else {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil {
				b.Tools = append(b.Tools, Tool{Slug: s, Label: titleCase(s)})
			}
		}
	}
	return nil
}

type ToolStatus struct {
	Tool
	Status  string `json:"status"` // downloading, running, stopped, error
	Message string `json:"message,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

var (
	bundle     Bundle
	statuses   []ToolStatus
	statusMu   sync.Mutex
	procs      []*exec.Cmd
	baseDir    string
	toolsDir   string
	dataDir    string
	dashPort   = 9700
)

// Port map for all known tools
var portMap = map[string]int{
	"headcount": 9100, "sentinel": 9101, "sundial": 9102, "corral": 9103,
	"campfire": 9104, "announcements": 9105, "silo": 9106, "roster": 9107,
	"outpost": 9108, "paddock": 9109, "mainspring": 9110, "tally": 9111,
	"dossier": 9200, "billfold": 9201, "steward": 9202, "roundup": 9203,
	"notebook": 9204, "surveyor": 9205, "ponyexpress": 9206, "deposition": 9207,
	"trailhead": 9208, "quartermaster": 9209, "agora": 9210, "prospector": 9211,
	"ledger": 9212, "checkout": 9213, "collection": 9214, "dispatch": 9215,
	"booking": 9800, "waiver": 9801, "estimate": 9802, "breeding": 9803,
	"tournament": 9804, "recipe": 9805, "reservation": 9806, "checkin": 9807,
	"portfolio": 9808, "fleet": 9809, "harvest": 9810, "permit": 9811,
	"menu": 9812, "curriculum": 9813,
}

func main() {
	bundleSlug := ""
	for i, a := range os.Args[1:] {
		if a == "-bundle" && i+1 < len(os.Args)-1 {
			bundleSlug = os.Args[i+2]
		}
		if a == "-port" && i+1 < len(os.Args)-1 {
			fmt.Sscanf(os.Args[i+2], "%d", &dashPort)
		}
	}
	if bundleSlug == "" {
		// Check for saved config
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, "stockyard")
		cfg, err := os.ReadFile(filepath.Join(baseDir, "bundle.json"))
		if err == nil {
			json.Unmarshal(cfg, &bundle)
			bundleSlug = bundle.Slug
		}
	}
	if bundleSlug == "" {
		fmt.Println("Stockyard Launcher v" + version)
		fmt.Println()
		fmt.Println("Usage: stockyard-launcher -bundle <slug>")
		fmt.Println()
		fmt.Println("Example: stockyard-launcher -bundle therapist")
		fmt.Println("         stockyard-launcher -bundle minecraft")
		fmt.Println()
		fmt.Println("Find your bundle: https://stockyard.dev/for/")
		os.Exit(1)
	}

	home, _ := os.UserHomeDir()
	baseDir = filepath.Join(home, "stockyard")
	toolsDir = filepath.Join(baseDir, "tools")
	dataDir = filepath.Join(baseDir, "data")
	os.MkdirAll(toolsDir, 0755)
	os.MkdirAll(dataDir, 0755)

	// Fetch bundle definition if not cached
	if bundle.Slug == "" {
		fmt.Printf("Fetching bundle: %s\n", bundleSlug)
		resp, err := http.Get("https://stockyard.dev/api/bundle/" + bundleSlug)
		if err != nil || resp.StatusCode != 200 {
			// Try loading from bundles list
			bundle = fetchBundleFromList(bundleSlug)
		} else {
			defer resp.Body.Close()
			json.NewDecoder(resp.Body).Decode(&bundle)
		}
		if bundle.Slug == "" {
			fmt.Println("Bundle not found: " + bundleSlug)
			os.Exit(1)
		}
		// Assign ports
		for i := range bundle.Tools {
			if p, ok := portMap[bundle.Tools[i].Slug]; ok {
				bundle.Tools[i].Port = p
			} else {
				bundle.Tools[i].Port = 9900 + i
			}
		}
		// Save config
		cfg, _ := json.MarshalIndent(bundle, "", "  ")
		os.WriteFile(filepath.Join(baseDir, "bundle.json"), cfg, 0644)
	}

	// Init statuses
	statuses = make([]ToolStatus, len(bundle.Tools))
	for i, t := range bundle.Tools {
		statuses[i] = ToolStatus{Tool: t, Status: "stopped"}
	}

	fmt.Printf("\n  Stockyard for %s\n", bundle.Name)
	fmt.Printf("  %d tools\n\n", len(bundle.Tools))

	// Download missing tools
	downloadTools()

	// Start all tools
	startAll()

	// Start dashboard
	go serveDashboard()

	fmt.Printf("\n  Dashboard: http://localhost:%d\n\n", dashPort)
	openBrowser(fmt.Sprintf("http://localhost:%d", dashPort))

	// Wait for signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nStopping all tools...")
	stopAll()
}

func fetchBundleFromList(slug string) Bundle {
	resp, err := http.Get("https://stockyard.dev/bundles-search.json")
	if err != nil {
		return Bundle{}
	}
	defer resp.Body.Close()
	var bundles []Bundle
	json.NewDecoder(resp.Body).Decode(&bundles)
	for _, b := range bundles {
		if b.Slug == slug {
			return b
		}
	}
	return Bundle{}
}

func downloadTools() {
	osName := runtime.GOOS
	arch := runtime.GOARCH
	var wg sync.WaitGroup

	for i, t := range bundle.Tools {
		binPath := filepath.Join(toolsDir, "stockyard-"+t.Slug)
		if _, err := os.Stat(binPath); err == nil {
			setStatus(i, "stopped", "ready")
			continue
		}
		wg.Add(1)
		go func(idx int, tool Tool) {
			defer wg.Done()
			setStatus(idx, "downloading", "")
			err := downloadTool(tool.Slug, osName, arch)
			if err != nil {
				setStatus(idx, "error", err.Error())
				fmt.Printf("  ✗ %s: %v\n", tool.Label, err)
			} else {
				setStatus(idx, "stopped", "ready")
				fmt.Printf("  ✓ %s\n", tool.Label)
			}
		}(i, t)
	}
	wg.Wait()
}

func downloadTool(slug, osName, arch string) error {
	url := fmt.Sprintf("https://github.com/stockyard-dev/stockyard-%s/releases/latest/download/stockyard-%s_%s_%s.tar.gz",
		slug, slug, osName, arch)

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		exeExt := ""
		if osName == "windows" {
			exeExt = ".exe"
		}
		dst := filepath.Join(toolsDir, "stockyard-"+slug+exeExt)
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return err
		}
		io.Copy(f, tr)
		f.Close()
		return nil
	}
	return fmt.Errorf("no binary in archive")
}

// downloadToolConfig fetches the personalization config.json for a tool
// and writes it to the tool's data directory. If a config already exists,
// it is preserved (so user edits are not overwritten).
func downloadToolConfig(bundleSlug, toolSlug, toolDataDir string) {
	cfgPath := filepath.Join(toolDataDir, "config.json")
	if _, err := os.Stat(cfgPath); err == nil {
		return // already exists, don't overwrite user changes
	}
	url := fmt.Sprintf("https://stockyard.dev/api/toolkit/%s/config/%s", bundleSlug, toolSlug)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return // no config available — totally fine, tool will use defaults
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || len(body) < 2 {
		return
	}
	os.WriteFile(cfgPath, body, 0644)
}

func startAll() {
	exeExt := ""
	if runtime.GOOS == "windows" {
		exeExt = ".exe"
	}
	for i, t := range bundle.Tools {
		binPath := filepath.Join(toolsDir, "stockyard-"+t.Slug+exeExt)
		if _, err := os.Stat(binPath); err != nil {
			continue
		}
		toolData := filepath.Join(dataDir, t.Slug)
		os.MkdirAll(toolData, 0755)

		// Download personalization config if not already present
		downloadToolConfig(bundle.Slug, t.Slug, toolData)

		cmd := exec.Command(binPath, "-port", fmt.Sprintf("%d", t.Port), "-data", toolData)
		cmd.Env = append(os.Environ(),
			fmt.Sprintf("PORT=%d", t.Port),
			fmt.Sprintf("DATA_DIR=%s", toolData),
		)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		err := cmd.Start()
		if err != nil {
			setStatus(i, "error", err.Error())
			continue
		}
		procs = append(procs, cmd)
		setStatus(i, "running", "")
		statuses[i].PID = cmd.Process.Pid

		// Monitor in background
		go func(idx int, c *exec.Cmd) {
			c.Wait()
			setStatus(idx, "stopped", "exited")
		}(i, cmd)
	}
}

func stopAll() {
	for _, cmd := range procs {
		if cmd.Process != nil {
			cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	time.Sleep(500 * time.Millisecond)
	for _, cmd := range procs {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}
}

func setStatus(idx int, status, msg string) {
	statusMu.Lock()
	defer statusMu.Unlock()
	statuses[idx].Status = status
	statuses[idx].Message = msg
}

func serveDashboard() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", dashboardPage)
	mux.HandleFunc("GET /api/status", apiStatus)
	mux.HandleFunc("POST /api/stop", apiStopAll)
	http.ListenAndServe(fmt.Sprintf(":%d", dashPort), mux)
}

func apiStatus(w http.ResponseWriter, r *http.Request) {
	statusMu.Lock()
	defer statusMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"bundle":  bundle,
		"tools":   statuses,
		"version": version,
	})
}

func apiStopAll(w http.ResponseWriter, r *http.Request) {
	go func() {
		stopAll()
		os.Exit(0)
	}()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopping"})
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	}
	if cmd != nil {
		cmd.Start()
	}
}

func dashboardPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Stockyard — %s</title>
<link href="https://fonts.googleapis.com/css2?family=Libre+Baskerville:ital,wght@0,400;0,700;1,400&family=JetBrains+Mono:wght@400;600&display=swap" rel="stylesheet">
<style>
:root{--bg:#1a1410;--bg2:#241e18;--bg3:#2e261e;--rust:#e8753a;--leather:#c4a87a;--cream:#f0e6d3;--cd:#bfb5a3;--cm:#7a7060;--gold:#d4a843;--green:#4a9e5c;--red:#c94444;--mono:'JetBrains Mono',monospace;--serif:'Libre Baskerville',serif}
*{margin:0;padding:0;box-sizing:border-box}body{background:var(--bg);color:var(--cream);font-family:var(--serif);line-height:1.6;padding:2rem}
.hdr{text-align:center;margin-bottom:2rem}.hdr h1{font-family:var(--mono);font-size:1.1rem;letter-spacing:2px;color:var(--rust)}.hdr p{font-size:.85rem;color:var(--cd);margin-top:.3rem}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:.8rem;max-width:1000px;margin:0 auto}
.card{background:var(--bg2);border:1px solid var(--bg3);padding:1rem 1.2rem;transition:border-color .2s;cursor:pointer;text-decoration:none;color:inherit;display:block}
.card:hover{border-color:var(--leather)}
.card h3{font-family:var(--mono);font-size:.8rem;margin-bottom:.3rem;display:flex;justify-content:space-between;align-items:center}
.card p{font-size:.75rem;color:var(--cd)}
.dot{width:8px;height:8px;border-radius:50%;display:inline-block}
.dot.running{background:var(--green)}.dot.downloading{background:var(--gold);animation:pulse 1s infinite}.dot.error{background:var(--red)}.dot.stopped{background:var(--cm)}
@keyframes pulse{0%%,100%%{opacity:1}50%%{opacity:.4}}
.port{font-family:var(--mono);font-size:.6rem;color:var(--cm);margin-top:.4rem}
.foot{text-align:center;margin-top:2rem;font-family:var(--mono);font-size:.65rem;color:var(--cm)}
.foot button{background:none;border:1px solid var(--bg3);color:var(--cd);font-family:var(--mono);font-size:.6rem;padding:.3rem .8rem;cursor:pointer;margin-top:.5rem}
.foot button:hover{border-color:var(--red);color:var(--red)}
</style></head><body>
<div class="hdr"><h1>STOCKYARD</h1><p>%s — %d tools running on your machine</p></div>
<div class="grid" id="grid"></div>
<div class="foot"><p>All data stored in ~/stockyard/data/</p><button onclick="fetch('/api/stop',{method:'POST'}).then(function(){document.body.innerHTML='<div style=padding:4rem;text-align:center;font-family:var(--mono)>Stopped. You can close this window.</div>'})">Stop All Tools</button></div>
<script>
function load(){
fetch('/api/status').then(function(r){return r.json()}).then(function(d){
var h='';d.tools.forEach(function(t){
h+='<a class="card" href="http://localhost:'+t.port+'/ui" target="_blank">';
h+='<h3>'+t.label+' <span class="dot '+t.status+'"></span></h3>';
h+='<p>'+t.desc+'</p>';
h+='<div class="port">localhost:'+t.port+'</div>';
h+='</a>';
});
document.getElementById('grid').innerHTML=h;
})}
load();setInterval(load,3000);
</script></body></html>`, bundle.Name, bundle.Name, len(bundle.Tools))
}

func titleCase(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
