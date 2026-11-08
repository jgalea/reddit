package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const usage = `reddit - search and read Reddit by loading its pages in your own Chrome profile

  login                       sign in by hand, once
  status                      whether the saved profile holds a Reddit session
  search QUERY [--sub NAME] [--sort relevance|new|top|comments] [--time hour|day|week|month|year|all]
  thread URL                  a post with its comments (page text)
  sub NAME [--sort hot|new|top] [--time ...]
  page URL                    the text of any reddit.com page (waits for it to load)
  research QUERY [--also "q2|q3"] [--sub NAME] [--threads N] [--terms a,b,c] [--max N]
                              search, read the top threads in one go, and tally how often each --terms name is mentioned
  sentiment NAME [--also "q2|q3"] [--sub NAME] [--threads N]
                              the comment lines that mention NAME, split by cue-word polarity (a rough pass; read the lines)
  replies                     your inbox (replies to your posts and comments)

Search and sub print each result's title and link. thread and replies print the page text.
--max N caps the characters printed (default 20000); reads run in a background browser (--show for a visible window).
Environment: REDDIT_CHROME (browser binary), REDDIT_HOME (default ~/.reddit)`

func home() string {
	if h := os.Getenv("REDDIT_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".reddit")
}

func chromePath() string {
	if p := os.Getenv("REDDIT_CHROME"); p != "" {
		return p
	}
	for _, p := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// openBackground starts the normal (visible-mode) Chrome hidden and without
// taking focus, then attaches to it. Reddit refuses headless Chrome.
func frontApp() (asn, name, bundle string) {
	out, err := exec.Command("lsappinfo", "front").Output()
	if err != nil {
		return
	}
	asn = strings.TrimSpace(string(out))
	field := func(key string) string {
		o, _ := exec.Command("lsappinfo", "info", "-only", key, asn).Output()
		parts := strings.Split(strings.TrimSpace(string(o)), "=")
		return strings.Trim(parts[len(parts)-1], `"`)
	}
	return asn, field("name"), field("bundleid")
}

// guardFocus hands focus back to the app the user was in whenever the
// background browser takes it. It stops when the returned func is called.
func guardFocus(browserName string) func() {
	_, origName, origBundle := frontApp()
	done := make(chan struct{})
	if origBundle == "" || origName == browserName {
		return func() {}
	}
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(25 * time.Millisecond):
			}
			if _, name, _ := frontApp(); name == browserName {
				exec.Command("open", "-b", origBundle).Run()
			}
		}
	}()
	return func() { close(done) }
}

// hideBrowser hides our Chrome instance (found by its profile) like Cmd-H.
func hideBrowser(profile string) {
	out, err := exec.Command("pgrep", "-o", "-f", "user-data-dir="+profile).Output()
	pid := strings.TrimSpace(string(out))
	if err != nil || pid == "" {
		return
	}
	exec.Command("osascript", "-e",
		`tell application "System Events" to set visible of (first process whose unix id is `+pid+`) to false`).Run()
}

// parkWindow rewrites the profile's remembered window position off-screen,
// since Chrome restores that instead of honouring --window-position.
func parkWindow(profile string) {
	path := filepath.Join(profile, "Default", "Preferences")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var prefs map[string]any
	if json.Unmarshal(b, &prefs) != nil {
		return
	}
	br, _ := prefs["browser"].(map[string]any)
	if br == nil {
		br = map[string]any{}
		prefs["browser"] = br
	}
	br["window_placement"] = map[string]any{
		"left": -3000, "top": -3000, "right": -1720, "bottom": -2100, "maximized": false,
	}
	if out, err := json.Marshal(prefs); err == nil {
		os.WriteFile(path, out, 0o600)
	}
}

func openBackground(timeout time.Duration) (context.Context, func(), error) {
	profile := filepath.Join(home(), "profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, nil, err
	}
	portFile := filepath.Join(profile, "DevToolsActivePort")
	os.Remove(portFile)
	app := "Google Chrome"
	if p := os.Getenv("REDDIT_CHROME_APP"); p != "" {
		app = p
	}
	parkWindow(profile)
	stopGuard := guardFocus(app)
	err := exec.Command("open", "-g", "-j", "-n", "-a", app, "--args",
		"--remote-debugging-port=0",
		"--user-data-dir="+profile, "--no-first-run", "--no-default-browser-check",
		"--window-size=1280,900",
		// park the window off-screen so it never shows over the user's work,
		// and keep the hidden page rendering at full speed
		"--window-position=-3000,-3000",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-background-timer-throttling").Run()
	if err != nil {
		return nil, nil, fmt.Errorf("could not start %s in the background: %w", app, err)
	}
	var wsURL string
	for i := 0; i < 40 && wsURL == ""; i++ {
		time.Sleep(500 * time.Millisecond)
		b, err := os.ReadFile(portFile)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]))
		if err != nil {
			continue
		}
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
		if err != nil {
			continue
		}
		var v struct {
			WS string `json:"webSocketDebuggerUrl"`
		}
		json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		wsURL = v.WS
	}
	if wsURL == "" {
		return nil, nil, fmt.Errorf("background Chrome did not come up; is another Chrome using %s?", profile)
	}
	var cancels []context.CancelFunc
	ctx, c := chromedp.NewRemoteAllocator(context.Background(), wsURL)
	cancels = append(cancels, c)
	ctx, c = chromedp.NewContext(ctx)
	cancels = append(cancels, c)
	if timeout > 0 {
		ctx, c = context.WithTimeout(ctx, timeout)
		cancels = append(cancels, c)
	}
	// macOS clamps windows back to the screen edge, so hide the whole app
	// once the tab exists; the flags above keep hidden pages running.
	chromedp.Run(ctx)
	hideBrowser(profile)
	return ctx, func() {
		defer stopGuard()
		// quit the whole browser so nothing keeps running
		chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			return browser.Close().Do(ctx)
		}))
		for i := len(cancels) - 1; i >= 0; i-- {
			cancels[i]()
		}
	}, nil
}

func open(headless bool, timeout time.Duration) (context.Context, func(), error) {
	exe := chromePath()
	if exe == "" {
		return nil, nil, fmt.Errorf("no Chrome found; set REDDIT_CHROME")
	}
	profile := filepath.Join(home(), "profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, nil, err
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(exe),
		chromedp.UserDataDir(profile),
		chromedp.Flag("headless", headless),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.WindowSize(1280, 900),
	)
	var cancels []context.CancelFunc
	ctx, c := chromedp.NewExecAllocator(context.Background(), opts...)
	cancels = append(cancels, c)
	ctx, c = chromedp.NewContext(ctx)
	cancels = append(cancels, c)
	if timeout > 0 {
		ctx, c = context.WithTimeout(ctx, timeout)
		cancels = append(cancels, c)
	}
	return ctx, func() {
		for i := len(cancels) - 1; i >= 0; i-- {
			cancels[i]()
		}
	}, nil
}

func cookiePath() string { return filepath.Join(home(), "cookies.json") }

// Reddit's login cookie is session-scoped, so Chrome drops it when the
// window closes; keep our own copy (0600) and give it a month's life.
func saveCookies(ctx context.Context) (int, error) {
	var cs []*network.Cookie
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cs, err = network.GetCookies().WithURLs([]string{"https://www.reddit.com/"}).Do(ctx)
		return err
	}))
	if err != nil {
		return 0, err
	}
	b, err := json.Marshal(cs)
	if err != nil {
		return 0, err
	}
	return len(cs), os.WriteFile(cookiePath(), b, 0o600)
}

func loadCookies(ctx context.Context) error {
	b, err := os.ReadFile(cookiePath())
	if err != nil {
		return nil
	}
	var cs []*network.Cookie
	if err := json.Unmarshal(b, &cs); err != nil {
		return err
	}
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		for _, c := range cs {
			exp := cdp.TimeSinceEpoch(time.Now().Add(30 * 24 * time.Hour))
			if !c.Session {
				exp = cdp.TimeSinceEpoch(time.Unix(int64(c.Expires), 0))
			}
			err := network.SetCookie(c.Name, c.Value).WithDomain(c.Domain).WithPath(c.Path).
				WithSecure(c.Secure).WithHTTPOnly(c.HTTPOnly).WithExpires(&exp).Do(ctx)
			if err != nil {
				return err
			}
		}
		return nil
	}))
}

func hasSessionCookie(ctx context.Context) bool {
	var found bool
	chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		cs, err := network.GetCookies().WithURLs([]string{"https://www.reddit.com/"}).Do(ctx)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.Name == "reddit_session" {
				found = true
			}
		}
		return nil
	}))
	return found
}

func login() error {
	ctx, closeFn, err := open(false, 0)
	if err != nil {
		return err
	}
	defer closeFn()
	if err := chromedp.Run(ctx, chromedp.Navigate("https://www.reddit.com/login/")); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "A Chrome window is opening. Sign in to Reddit there.")
	fmt.Fprintln(os.Stderr, "It picks the session up on its own; press Enter here if it has not noticed.")
	enter := make(chan struct{})
	go func() {
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err == nil {
			close(enter)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-enter:
		case <-time.After(3 * time.Second):
		}
		var loc string
		if err := chromedp.Run(ctx, chromedp.Location(&loc)); err != nil {
			return err
		}
		if strings.Contains(loc, "/login") || strings.Contains(loc, "/register") {
			continue
		}
		if hasSessionCookie(ctx) {
			// let Chrome write the cookies to the profile before the window closes
			chromedp.Run(ctx, chromedp.Navigate("https://www.reddit.com/settings/"), chromedp.Sleep(12*time.Second))
			n, err := saveCookies(ctx)
			if err != nil {
				return fmt.Errorf("signed in, but saving the session failed: %w", err)
			}
			fmt.Fprintf(os.Stderr, "Signed in. Saved %d cookies to %s.\n", n, cookiePath())
			return nil
		}
	}
}

const extractJS = `(() => {
  const seen = new Set(), rows = [];
  document.querySelectorAll('a[href*="/comments/"]').forEach(a => {
    if (!/(^|\.)reddit\.com$/.test(a.hostname)) return;
    const href = a.href.split('?')[0];
    const m = href.match(/\/r\/([^/]+)\/comments\/([^/]+)\/[^/]+\/?$/);
    if (!m || seen.has(href)) return;
    const t = (a.innerText || a.getAttribute('aria-label') || '').trim();
    if (!t) return;
    seen.add(href);
    rows.push({sub: m[1], title: t.split('\n')[0], url: href});
  });
  return JSON.stringify(rows);
})()`

type result struct {
	Sub   string `json:"sub"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

func loadPage(ctx context.Context, u string, js string, out any) error {
	return chromedp.Run(ctx,
		chromedp.Navigate(u),
		chromedp.Sleep(5*time.Second),
		chromedp.Evaluate(js, out),
	)
}

func parseResults(raw string) []result {
	var rows []result
	json.Unmarshal([]byte(raw), &rows)
	return rows
}

func printResults(raw string, limit int) {
	rows := parseResults(raw)
	if len(rows) == 0 {
		fmt.Println("no results")
		return
	}
	for i, r := range rows {
		if limit > 0 && i >= limit {
			break
		}
		fmt.Printf("r/%s  %s\n  %s\n\n", r.Sub, r.Title, r.URL)
	}
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if max > 0 && len(s) > max {
		return s[:max] + "\n[cut at " + fmt.Sprint(max) + " characters; raise --max]"
	}
	return s
}

func cleanThreadURL(arg string) (string, error) {
	if !strings.HasPrefix(arg, "http") {
		return "https://www.reddit.com/comments/" + strings.TrimPrefix(arg, "t3_") + "/", nil
	}
	u, err := url.Parse(arg)
	if err != nil {
		return "", err
	}
	if h := u.Hostname(); h != "reddit.com" && !strings.HasSuffix(h, ".reddit.com") {
		return "", fmt.Errorf("%s is not a reddit.com link", arg)
	}
	return "https://www.reddit.com" + u.Path, nil
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Println(usage)
		return
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "reddit:", err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	var sub, sort, tm string
	var limit, max int
	var show bool
	var threads int
	var terms, also string
	fs.IntVar(&threads, "threads", 8, "research: threads to read")
	fs.StringVar(&terms, "terms", "", "research: comma-separated names to count across the threads (brands, models)")
	fs.StringVar(&also, "also", "", "research: extra search queries, separated by |")
	fs.StringVar(&sub, "sub", "", "restrict to one subreddit")
	fs.StringVar(&sort, "sort", "", "sort order")
	fs.StringVar(&tm, "time", "", "time window")
	fs.IntVar(&limit, "limit", 15, "results to print")
	fs.IntVar(&max, "max", 20000, "characters of page text to print")
	fs.BoolVar(&show, "show", false, "open a visible browser window instead of the background one")
	var pos []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		rest = fs.Args()
		if len(rest) > 0 {
			pos = append(pos, rest[0])
			rest = rest[1:]
		}
	}
	if cmd == "login" {
		return login()
	}

	openFn := func() (context.Context, func(), error) { return openBackground(120 * time.Second) }
	if show || runtime.GOOS != "darwin" {
		openFn = func() (context.Context, func(), error) { return open(false, 120*time.Second) }
	}
	ctx, closeFn, err := openFn()
	if err != nil {
		return err
	}
	defer closeFn()

	if err := loadCookies(ctx); err != nil {
		return err
	}
	var target string
	switch cmd {
	case "status":
		if err := chromedp.Run(ctx, chromedp.Navigate("https://www.reddit.com/")); err != nil {
			return err
		}
		if hasSessionCookie(ctx) {
			fmt.Println("the profile holds a Reddit session")
			return nil
		}
		fmt.Println("no Reddit session; run `reddit login`")
		return fmt.Errorf("no session")
	case "search":
		if len(pos) == 0 {
			return fmt.Errorf("search needs a query")
		}
		target = searchTarget(strings.Join(pos, " "), sub, sort, tm)
	case "research":
		if len(pos) == 0 {
			return fmt.Errorf("research needs a query")
		}
		queries := append([]string{strings.Join(pos, " ")}, splitList(also, "|")...)
		return research(ctx, queries, sub, sort, tm, threads, splitList(terms, ","), max)
	case "sentiment":
		if len(pos) == 0 {
			return fmt.Errorf("sentiment needs a product or brand name")
		}
		term := strings.Join(pos, " ")
		return sentiment(ctx, term, append([]string{term}, splitList(also, "|")...), sub, sort, tm, threads)
	case "sub":
		if len(pos) == 0 {
			return fmt.Errorf("sub needs a subreddit name")
		}
		target = "https://www.reddit.com/r/" + strings.TrimPrefix(pos[0], "r/") + "/" + firstNonEmpty(sort, "hot") + "/"
		if tm != "" {
			target += "?t=" + tm
		}
	case "thread":
		if len(pos) == 0 {
			return fmt.Errorf("thread needs a URL or id")
		}
		if target, err = cleanThreadURL(pos[0]); err != nil {
			return err
		}
		var txt string
		if err := loadPage(ctx, target, `document.body.innerText`, &txt); err != nil {
			return err
		}
		fmt.Println(clip(txt, max))
		return nil
	case "page":
		if len(pos) == 0 {
			return fmt.Errorf("page needs a reddit.com URL")
		}
		if target, err = cleanThreadURL(pos[0]); err != nil {
			return err
		}
		var txt string
		if err := chromedp.Run(ctx, chromedp.Navigate(target), chromedp.Sleep(9*time.Second), chromedp.Evaluate(`document.body.innerText`, &txt)); err != nil {
			return err
		}
		fmt.Println(clip(txt, max))
		return nil
	case "replies":
		var txt string
		if err := chromedp.Run(ctx, chromedp.Navigate("https://www.reddit.com/message/inbox/"), chromedp.Sleep(9*time.Second), chromedp.Evaluate(`document.body.innerText`, &txt)); err != nil {
			return err
		}
		fmt.Println(clip(txt, max))
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}

	var raw string
	if err := loadPage(ctx, target, extractJS, &raw); err != nil {
		return err
	}
	printResults(raw, limit)
	return nil
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func searchTarget(query, sub, sort, tm string) string {
	q := url.Values{"q": {query}}
	if sort != "" {
		q.Set("sort", sort)
	}
	if tm != "" {
		q.Set("t", tm)
	}
	if sub != "" {
		q.Set("restrict_sr", "1")
		return "https://www.reddit.com/r/" + strings.TrimPrefix(sub, "r/") + "/search/?" + q.Encode()
	}
	return "https://www.reddit.com/search/?" + q.Encode()
}

func splitList(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

type tally struct {
	Term     string
	Threads  int
	Mentions int
}

// countTerms counts, case-insensitively, how many threads mention each term and how many times in all.
func countTerms(texts map[string]string, terms []string) []tally {
	var out []tally
	for _, t := range terms {
		row := tally{Term: t}
		lt := strings.ToLower(t)
		for _, txt := range texts {
			if n := strings.Count(strings.ToLower(txt), lt); n > 0 {
				row.Threads++
				row.Mentions += n
			}
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Threads != out[j].Threads {
			return out[i].Threads > out[j].Threads
		}
		return out[i].Mentions > out[j].Mentions
	})
	return out
}

func gather(ctx context.Context, queries []string, sub, sortBy, tm string, threads int) ([]result, map[string]string, error) {
	var found []result
	seen := map[string]bool{}
	for _, q := range queries {
		var raw string
		if err := loadPage(ctx, searchTarget(q, sub, sortBy, tm), extractJS, &raw); err != nil {
			return nil, nil, err
		}
		for _, r := range parseResults(raw) {
			clean, err := cleanThreadURL(r.URL)
			if err != nil || seen[clean] {
				continue
			}
			seen[clean] = true
			r.URL = clean
			found = append(found, r)
		}
	}
	if threads > 0 && len(found) > threads {
		found = found[:threads]
	}
	texts := map[string]string{}
	for _, r := range found {
		var txt string
		var err error
		for try := 0; try < 3; try++ {
			if err = loadPage(ctx, r.URL, `document.body.innerText`, &txt); err == nil {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipped %s: %v\n", r.URL, err)
			continue
		}
		texts[r.URL] = txt
	}
	if len(texts) == 0 {
		return nil, nil, fmt.Errorf("no thread could be read")
	}
	return found, texts, nil
}

func research(ctx context.Context, queries []string, sub, sortBy, tm string, threads int, terms []string, max int) error {
	found, texts, err := gather(ctx, queries, sub, sortBy, tm, threads)
	if err != nil {
		return err
	}
	if len(terms) > 0 {
		fmt.Printf("Mentions across %d threads:\n", len(texts))
		for _, t := range countTerms(texts, terms) {
			fmt.Printf("  %-24s %2d threads  %3d mentions\n", t.Term, t.Threads, t.Mentions)
		}
		fmt.Println()
	}
	for _, r := range found {
		if txt, ok := texts[r.URL]; ok {
			fmt.Printf("=== r/%s  %s\n%s\n\n%s\n\n", r.Sub, r.Title, r.URL, clip(txt, max))
		}
	}
	return nil
}

var positiveWords = []string{"love", "great", "amazing", "awesome", "solid", "reliable", "recommend", "happy", "no problems", "never had", "held up", "perfect", "best", "fantastic", "good value", "fits", "worth it", "excellent", "can't complain"}
var negativeWords = []string{"trash", "garbage", "junk", "suck", "puff", "swell", "burn", "fire", "died", "dead", "unbalanced", "don't balance", "sag", "overpriced", "avoid", "cheap for a reason", "worst", "disappoint", "regret", "flimsy", "fail", "dnf", "bad", "cheaped out", "not worth"}

type snippet struct {
	Text  string
	URL   string
	Score int
}

// polarity counts positive minus negative cue words in a comment line.
func polarity(line string) int {
	l := strings.ToLower(line)
	n := 0
	for _, w := range positiveWords {
		if strings.Contains(l, w) {
			n++
		}
	}
	for _, w := range negativeWords {
		if strings.Contains(l, w) {
			n--
		}
	}
	return n
}

// snippetsFor returns every comment line that mentions term, with its cue-word polarity.
func snippetsFor(texts map[string]string, term string) []snippet {
	lt := strings.ToLower(term)
	var out []snippet
	urls := make([]string, 0, len(texts))
	for u := range texts {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	for _, u := range urls {
		for _, line := range strings.Split(texts[u], "\n") {
			line = strings.TrimSpace(line)
			if len(line) < 15 || len(line) > 700 || !strings.Contains(strings.ToLower(line), lt) {
				continue
			}
			out = append(out, snippet{Text: line, URL: u, Score: polarity(line)})
		}
	}
	return out
}

func sentiment(ctx context.Context, term string, queries []string, sub, sortBy, tm string, threads int) error {
	_, texts, err := gather(ctx, queries, sub, sortBy, tm, threads)
	if err != nil {
		return err
	}
	sn := snippetsFor(texts, term)
	var pos, neg, neu []snippet
	for _, s := range sn {
		switch {
		case s.Score > 0:
			pos = append(pos, s)
		case s.Score < 0:
			neg = append(neg, s)
		default:
			neu = append(neu, s)
		}
	}
	fmt.Printf("%q in %d threads: %d comment lines, %d positive, %d negative, %d neutral (cue-word count, rough; read the lines)\n\n", term, len(texts), len(sn), len(pos), len(neg), len(neu))
	show := func(title string, list []snippet, desc bool) {
		if len(list) == 0 {
			return
		}
		sort.SliceStable(list, func(i, j int) bool {
			if desc {
				return list[i].Score > list[j].Score
			}
			return list[i].Score < list[j].Score
		})
		fmt.Println(title)
		for i, s := range list {
			if i >= 6 {
				break
			}
			fmt.Printf("  - %s\n    %s\n", clip(s.Text, 300), s.URL)
		}
		fmt.Println()
	}
	show("Positive:", pos, true)
	show("Negative:", neg, false)
	return nil
}
