// Command markham-schedule scrapes the City of Markham drop-in schedule and serves it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"
	_ "time/tzdata"

	"github.com/vBuilt/markham-schedule/internal/api"
	"github.com/vBuilt/markham-schedule/internal/auth"
	"github.com/vBuilt/markham-schedule/internal/config"
	"github.com/vBuilt/markham-schedule/internal/perfectmind"
	"github.com/vBuilt/markham-schedule/internal/remind"
	"github.com/vBuilt/markham-schedule/internal/schedule"
	"github.com/vBuilt/markham-schedule/internal/scraper"
	"github.com/vBuilt/markham-schedule/internal/store"
	"github.com/vBuilt/markham-schedule/internal/watch"
)

var version = "dev"

const (
	baseURL  = "https://cityofmarkham.perfectmind.com"
	widgetID = "6825ea71-e5b7-4c2a-948f-9195507ad90a"
)

func main() {
	once := flag.Bool("once", false, "run one refresh, print a summary, and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	health := flag.Bool("healthcheck", false, "probe /healthz on the local server and exit 0/1")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	if *health {
		os.Exit(healthcheck(cfg.ListenAddr))
	}
	os.Exit(run(cfg, *once))
}

func healthcheck(listen string) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: bad LISTEN_ADDR:", err)
		return 1
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

func logLevel(s string) slog.Level {
	var l slog.Level
	_ = l.UnmarshalText([]byte(s))
	return l
}

func run(cfg config.Config, once bool) int {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)}))
	slog.SetDefault(log)
	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		log.Error("load timezone", "err", err)
		return 1
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Error("create data dir", "err", err)
		return 1
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "schedule.db"))
	if err != nil {
		log.Error("open store", "err", err)
		return 1
	}
	defer st.Close()

	pm := perfectmind.New(perfectmind.Options{
		BaseURL:   baseURL,
		WidgetID:  widgetID,
		UserAgent: "Mozilla/5.0 (compatible; markham-schedule/" + version + "; +https://github.com/vBuilt/markham-schedule)",
		Delay:     cfg.RequestDelay,
		Logger:    log,
	})
	sc := scraper.New(pm, st, scraper.Config{
		Horizon: cfg.Horizon, Retention: cfg.Retention, Interval: cfg.RefreshInterval, Loc: loc,
		RegLookups: regLookups(cfg.RegLookups),
	}, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if once {
		status, err := sc.Refresh(ctx)
		return printSummary(status, err)
	}

	scDone := make(chan struct{})
	go func() { defer close(scDone); sc.Run(ctx) }()
	// Join the scraper before the deferred st.Close on every return path.
	defer func() { stop(); <-scDone }()

	mailer, bot := buildMailerAndBot(cfg, log)
	publicURL := cfg.BaseURL
	if publicURL == "" {
		_, port, err := net.SplitHostPort(cfg.ListenAddr)
		if err != nil {
			log.Error("bad LISTEN_ADDR", "err", err)
			return 1
		}
		publicURL = "http://localhost:" + port
		log.Warn("BASE_URL not set: links in emails point at "+publicURL, "base_url", publicURL)
	}
	authSvc := auth.NewService(st, auth.Options{
		BaseURL: publicURL, APIToken: cfg.APIToken, Mailer: mailer, Bot: bot, Log: log,
		AdminEmails: cfg.AdminEmails,
	})
	authHTTP := authSvc.Handler(auth.HTTPOptions{
		BaseURL: publicURL, Trusted: cfg.TrustedProxies, TurnstileSiteKey: cfg.TurnstileSiteKey,
	})
	cleanDone := make(chan struct{})
	go func() { defer close(cleanDone); cleanupLoop(ctx, authSvc, log) }()
	// Join the cleanup loop before the deferred st.Close on every return path.
	defer func() { stop(); <-cleanDone }()

	pmWatch := perfectmind.New(perfectmind.Options{
		BaseURL:   baseURL,
		WidgetID:  widgetID,
		UserAgent: "Mozilla/5.0 (compatible; markham-schedule/" + version + "; +https://github.com/vBuilt/markham-schedule)",
		Delay:     0,
		Logger:    log,
	})
	checker := watch.New(pmWatch, st, mailer, watch.Config{
		Interval: cfg.WatchInterval, Loc: loc, BaseURL: publicURL, APIToken: cfg.APIToken, Log: log,
	})
	watchDone := make(chan struct{})
	go func() { defer close(watchDone); checker.Run(ctx) }()
	// Join the checker before the deferred st.Close on every return path.
	defer func() { stop(); <-watchDone }()
	sender := remind.New(st, mailer, remind.Config{
		Loc: loc, BaseURL: publicURL, APIToken: cfg.APIToken, Log: log,
	})
	remindDone := make(chan struct{})
	go func() { defer close(remindDone); sender.Run(ctx) }()
	defer func() { stop(); <-remindDone }()
	log.Info("registration reminders on")
	log.Info("spot watching on", "interval", cfg.WatchInterval, "admins", len(cfg.AdminEmails))

	h := api.New(st, api.Options{
		Token: cfg.APIToken, CalName: "Markham Drop-In", Version: version,
		Refresh: cfg.RefreshInterval, Retention: cfg.Retention, FeedDays: 60, Loc: loc,
		Refresher: sc, Auth: authHTTP, Watch: watch.NewHTTP(checker, st, authHTTP),
		Remind: remind.NewHTTP(st, authHTTP, nil),
	})
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.ListenAddr, "version", version)

	select {
	case err := <-errc:
		log.Error("server", "err", err)
		return 1
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("shutdown", "err", err)
		return 1
	}
	return 0
}

// buildMailerAndBot picks the real email and bot-check services when their
// credentials are set, and loud stand-ins otherwise.
func buildMailerAndBot(cfg config.Config, log *slog.Logger) (auth.Mailer, auth.BotCheck) {
	var mailer auth.Mailer = &auth.ResendMailer{APIKey: cfg.ResendAPIKey, From: cfg.MailFrom}
	if cfg.ResendAPIKey == "" {
		log.Warn("RESEND_API_KEY not set: emails are written to the log, not sent")
		mailer = auth.LogMailer{Log: log}
	}
	// config.Load guarantees the Turnstile keys are both set or both empty, so
	// when the check is off the pages also get an empty site key.
	var bot auth.BotCheck = &auth.Turnstile{Secret: cfg.TurnstileSecret}
	if cfg.TurnstileSecret == "" {
		log.Warn("TURNSTILE_SITE_KEY and TURNSTILE_SECRET not set: bot protection is off")
		bot = auth.NoBotCheck{}
	}
	return mailer, bot
}

// cleanupLoop purges expired sessions, tokens and stale accounts, and prunes
// aged-out rate-limit entries, hourly until ctx is done.
func cleanupLoop(ctx context.Context, svc *auth.Service, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := svc.Cleanup(ctx); err != nil && ctx.Err() == nil {
			log.Error("auth cleanup", "err", err)
		}
		svc.Prune()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func printSummary(s schedule.Status, refreshErr error) int {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CATEGORY\tOK\tCOUNT\tERROR")
	okCount := 0
	for _, c := range s.Categories {
		if c.OK {
			okCount++
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", c.Name, strconv.FormatBool(c.OK), c.Count, c.Error)
	}
	fmt.Fprintf(tw, "TOTAL\t%d/%d ok\t%d\t%s\n", okCount, len(s.Categories), s.Total, s.Error)
	tw.Flush()
	if refreshErr != nil {
		fmt.Fprintln(os.Stderr, "refresh error:", refreshErr)
	}
	if okCount == 0 {
		return 1
	}
	return 0
}

// regLookups maps the config value (0 = off) onto scraper.Config (negative = off).
func regLookups(n int) int {
	if n == 0 {
		return -1
	}
	return n
}
