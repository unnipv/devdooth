// Command devdooth runs a Devdooth coordinator, a worker, or talks to a
// coordinator from the command line.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/unnipv/devdooth/internal/coordinator"
	"github.com/unnipv/devdooth/internal/worker"
)

func main() {
	log.SetFlags(log.Ltime)
	worker.Version = effectiveVersion()
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "coordinator":
		err = runCoordinator(os.Args[2:])
	case "worker":
		err = runWorker(os.Args[2:])
	case "join":
		err = runJoin(os.Args[2:])
	case "enroll-token":
		err = runEnrollToken(os.Args[2:])
	case "devices":
		err = runDevices(os.Args[2:])
	case "nodes":
		err = runNodes(os.Args[2:])
	case "lease":
		err = runLease(os.Args[2:])
	case "release":
		err = runRelease(os.Args[2:])
	case "pause":
		err = runPauseResume("pause", os.Args[2:])
	case "resume":
		err = runPauseResume("resume", os.Args[2:])
	case "mcp":
		err = runMCP(os.Args[2:])
	case "version", "--version", "-version":
		fmt.Println("devdooth", effectiveVersion())
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

var version = "0.0.0-dev"

// effectiveVersion reports the release version. Release binaries get it from
// -ldflags; binaries installed with `go install ...@vX.Y.Z` do not, so fall back
// to the module version recorded in the build info.
func effectiveVersion() string {
	if version != "0.0.0-dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return version
}

func usage() {
	fmt.Fprint(os.Stderr, `Devdooth — turn machines you own into a browser pool for AI agents.

Usage:
  devdooth coordinator [--addr :8080] [--store devdooth.db] [--admin-token T] [--worker-token T]
  devdooth join        --coordinator URL --enroll-token T [--name N] [--data-dir DIR]
  devdooth enroll-token --url URL --token ADMIN [--label L] [--ttl SECONDS]
  devdooth devices     --url URL --token ADMIN
  devdooth devices revoke --url URL --token ADMIN --id DEVICE_ID
  devdooth worker      --coordinator URL [--enroll-token T | --token T] [--name N]
                       [--data-dir DIR] [--max-slots N] [--headful] [--browser chrome]
                       [--profiles a,b] [--no-sandbox]
  devdooth nodes       --url URL --token ADMIN
  devdooth lease       --url URL --token ADMIN
                       [--node NAME] [--profile NAME] [--browser chrome]
                       [--headful] [--ttl 1800] [--start-url URL]
  devdooth release     --url URL --token ADMIN --lease ID
  devdooth pause       --url URL --token ADMIN --lease ID   # hand a headful session to a human
  devdooth resume      --url URL --token ADMIN --lease ID
  devdooth mcp         --url URL --token ADMIN
                       [--node N] [--profile P] [--headful] [--ttl 3600]
                       [--mcp "npx -y @playwright/mcp@0.0.83"] [-- extra mcp args]
  devdooth version

Environment:
  DEVDOOTH_ADMIN_TOKEN, DEVDOOTH_WORKER_TOKEN, DEVDOOTH_PUBLIC_URL, DEVDOOTH_ENROLL_TOKEN
`)
}

func env(key string) string { return os.Getenv(key) }

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runCoordinator(args []string) error {
	fs := newFlagSet("coordinator")
	addr := fs.String("addr", ":8080", "listen address")
	storePath := fs.String("store", "devdooth.db", "SQLite metadata path (\"\" disables durable identity)")
	adminToken := fs.String("admin-token", env("DEVDOOTH_ADMIN_TOKEN"), "admin/caller API token (generated if empty)")
	workerToken := fs.String("worker-token", env("DEVDOOTH_WORKER_TOKEN"), "static worker token for development (optional)")
	publicURL := fs.String("public-url", env("DEVDOOTH_PUBLIC_URL"), "public base URL used in lease endpoints")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *adminToken == "" {
		*adminToken = randomToken()
		fmt.Println("No --admin-token given. Generated one for this run:")
		fmt.Println("  DEVDOOTH_ADMIN_TOKEN=" + *adminToken)
		fmt.Println()
	}
	genWorkerToken := false
	if *workerToken == "" && *storePath == "" {
		*workerToken = randomToken()
		genWorkerToken = true
	}

	var store *coordinator.Store
	if *storePath != "" {
		s, err := coordinator.OpenStore(*storePath)
		if err != nil {
			return fmt.Errorf("open store %s: %w", *storePath, err)
		}
		defer s.Close()
		store = s
	}

	c := coordinator.New(coordinator.Options{
		AdminToken:  *adminToken,
		WorkerToken: *workerToken,
		PublicURL:   *publicURL,
		Store:       store,
		Logger:      log.Default(),
	})

	srv := &http.Server{Addr: *addr, Handler: c.Handler()}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	log.Printf("coordinator listening on %s", *addr)
	if genWorkerToken {
		log.Printf("worker token for this run: %s", *workerToken)
	}
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func runWorker(args []string) error {
	fs := newFlagSet("worker")
	coordinatorURL := fs.String("coordinator", "", "coordinator base URL, e.g. http://localhost:8080")
	token := fs.String("token", env("DEVDOOTH_TOKEN"), "static worker token (development)")
	enrollToken := fs.String("enroll-token", env("DEVDOOTH_ENROLL_TOKEN"), "single-use enrollment token; enrolls on first run")
	name := fs.String("name", "", "node name (defaults to hostname)")
	dataDir := fs.String("data-dir", "", "worker data directory (defaults to ~/.devdooth)")
	maxSlots := fs.Int("max-slots", 1, "maximum concurrent browser leases")
	headful := fs.Bool("headful", false, "advertise and default to headful browsers")
	browser := fs.String("browser", "", "preferred browser name (chrome, chromium, msedge)")
	profiles := fs.String("profiles", "", "comma-separated persistent profile names to create and advertise")
	noSandbox := fs.Bool("no-sandbox", false, "pass --no-sandbox to the browser (containers and some CI only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *coordinatorURL == "" {
		return fmt.Errorf("--coordinator is required")
	}
	if *name == "" {
		h, _ := os.Hostname()
		*name = h
	}

	w := worker.New(worker.Options{
		CoordinatorURL: *coordinatorURL,
		Token:          *token,
		EnrollToken:    *enrollToken,
		Name:           *name,
		DataDir:        *dataDir,
		MaxSlots:       *maxSlots,
		Headful:        *headful,
		Browser:        *browser,
		Profiles:       splitCSV(*profiles),
		NoSandbox:      *noSandbox,
		Logger:         log.Default(),
	})

	if bs := w.Browsers(); len(bs) > 0 {
		log.Printf("found %d browser(s):", len(bs))
		for _, b := range bs {
			log.Printf("  %s  %s  %s", b.Name, b.Path, b.Version)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := w.Run(ctx)
	if err == context.Canceled {
		return nil
	}
	return err
}

// ---------------------------------------------------------------- client commands

func runNodes(args []string) error {
	fs := newFlagSet("nodes")
	base := fs.String("url", "", "coordinator HTTP base URL")
	coord := fs.String("coordinator", "", "coordinator base URL (http or ws)")
	token := fs.String("token", env("DEVDOOTH_TOKEN"), "API token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	httpBase, err := resolveBase(*base, *coord)
	if err != nil {
		return err
	}
	body, code, err := apiRequest(http.MethodGet, httpBase+"/v1/nodes", *token, nil)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("coordinator returned %d: %s", code, strings.TrimSpace(string(body)))
	}
	var out struct {
		Nodes []struct {
			Name     string `json:"name"`
			OS       string `json:"os"`
			Arch     string `json:"arch"`
			Headful  bool   `json:"headful"`
			Slots    int    `json:"slots"`
			Active   int    `json:"active"`
			Online   bool   `json:"online"`
			Browsers []struct {
				Name string `json:"name"`
			} `json:"browsers"`
			Profiles []string `json:"profiles"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}
	if len(out.Nodes) == 0 {
		fmt.Println("no workers connected")
		return nil
	}
	fmt.Printf("%-16s %-14s %-10s %-8s %-8s %s\n", "NAME", "PLATFORM", "BROWSER", "SLOTS", "STATUS", "PROFILES")
	for _, n := range out.Nodes {
		browser := "-"
		if len(n.Browsers) > 0 {
			browser = n.Browsers[0].Name
		}
		status := "ready"
		if !n.Online {
			status = "offline"
		}
		profiles := strings.Join(n.Profiles, ",")
		if profiles == "" {
			profiles = "-"
		}
		fmt.Printf("%-16s %-14s %-10s %-8s %-8s %s\n",
			n.Name, n.OS+"/"+n.Arch, browser,
			fmt.Sprintf("%d/%d", n.Active, n.Slots), status, profiles)
	}
	return nil
}

func runLease(args []string) error {
	fs := newFlagSet("lease")
	base := fs.String("url", "", "coordinator HTTP base URL")
	coord := fs.String("coordinator", "", "coordinator base URL (http or ws)")
	token := fs.String("token", env("DEVDOOTH_TOKEN"), "API token")
	node := fs.String("node", "", "preferred node name")
	profile := fs.String("profile", "", "worker-local persistent profile name")
	browser := fs.String("browser", "", "requested browser name")
	headful := fs.Bool("headful", false, "request a headful browser")
	ttl := fs.Int("ttl", 1800, "lease TTL in seconds")
	startURL := fs.String("start-url", "", "URL to open on launch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	httpBase, err := resolveBase(*base, *coord)
	if err != nil {
		return err
	}
	req := map[string]any{
		"node": *node, "profile": *profile, "browser": *browser,
		"headful": *headful, "ttl_seconds": *ttl, "start_url": *startURL,
	}
	payload, _ := json.Marshal(req)
	body, code, err := apiRequest(http.MethodPost, httpBase+"/v1/leases", *token, payload)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("coordinator returned %d: %s", code, strings.TrimSpace(string(body)))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}
	pretty, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(pretty))
	if ep, ok := out["endpoint"].(string); ok && ep != "" {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Connect with Playwright:")
		fmt.Fprintln(os.Stderr, "  const browser = await chromium.connectOverCDP("+strconvQuote(ep)+")")
		fmt.Fprintln(os.Stderr, "Release with:")
		fmt.Fprintln(os.Stderr, "  devdooth release --url "+httpBase+" --token <token> --lease "+fmt.Sprint(out["lease_id"]))
	}
	return nil
}

func runPauseResume(action string, args []string) error {
	fs := newFlagSet(action)
	base := fs.String("url", "", "coordinator HTTP base URL")
	coord := fs.String("coordinator", "", "coordinator base URL (http or ws)")
	token := fs.String("token", env("DEVDOOTH_TOKEN"), "API token")
	lease := fs.String("lease", "", "lease id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *lease == "" {
		return fmt.Errorf("--lease is required")
	}
	httpBase, err := resolveBase(*base, *coord)
	if err != nil {
		return err
	}
	body, code, err := apiRequest(http.MethodPost, httpBase+"/v1/leases/"+*lease+"/"+action, *token, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("coordinator returned %d: %s", code, strings.TrimSpace(string(body)))
	}
	if action == "pause" {
		fmt.Printf("paused %s; a human can now use the browser on its worker. Resume with:\n  devdooth resume --url %s --token <token> --lease %s\n", *lease, httpBase, *lease)
		return nil
	}
	fmt.Printf("resumed %s; connect to the lease endpoint again to continue\n", *lease)
	return nil
}

func runRelease(args []string) error {
	fs := newFlagSet("release")
	base := fs.String("url", "", "coordinator HTTP base URL")
	coord := fs.String("coordinator", "", "coordinator base URL (http or ws)")
	token := fs.String("token", env("DEVDOOTH_TOKEN"), "API token")
	lease := fs.String("lease", "", "lease id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *lease == "" {
		return fmt.Errorf("--lease is required")
	}
	httpBase, err := resolveBase(*base, *coord)
	if err != nil {
		return err
	}
	body, code, err := apiRequest(http.MethodDelete, httpBase+"/v1/leases/"+*lease, *token, nil)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("coordinator returned %d: %s", code, strings.TrimSpace(string(body)))
	}
	fmt.Printf("released %s\n", *lease)
	return nil
}

// ---------------------------------------------------------------- helpers

func apiRequest(method, url, token string, payload []byte) ([]byte, int, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func resolveBase(base, coord string) (string, error) {
	raw := base
	if raw == "" {
		raw = coord
	}
	if raw == "" {
		return "", fmt.Errorf("--url or --coordinator is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("coordinator URL must be http(s) or ws(s)")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
