package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
)

var version = "0.2.1"

const (
	stateActive  = "active"
	stateStandby = "standby"
	stateError   = "error"
)

func main() {
	showVersion := flag.Bool("version", false, "Print version and exit")
	verbose := flag.Bool("verbose", false, "Enable debug logging")
	service := flag.String("service", envOrDefault("TS_SERVICE", "svc:ceph-mgr"), "Tailscale service name")
	upstream := flag.String("upstream", envOrDefault("TS_UPSTREAM", "127.0.0.1:8080"), "Local backend address")
	httpsPort := flag.String("https-port", envOrDefault("TS_HTTPS_PORT", "443"), "HTTPS listen port")
	tsSocket := flag.String("socket", envOrDefault("TS_SOCKET", ""), "Tailscale daemon socket path")
	cephAsok := flag.String("ceph-asok", envOrDefault("CEPH_ASOK", ""), "Ceph MGR admin socket path")
	defaultInterval, intervalEnvErr := envDurationOrDefault("POLL_INTERVAL", 30*time.Second)
	interval := flag.Duration("interval", defaultInterval, "Poll interval")
	flag.Parse()

	if *showVersion || (flag.NArg() > 0 && flag.Arg(0) == "version") {
		fmt.Println(version)
		return
	}

	if intervalEnvErr != nil {
		intervalFlagSet := false
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "interval" {
				intervalFlagSet = true
			}
		})
		if !intervalFlagSet {
			fmt.Fprintln(os.Stderr, intervalEnvErr)
			os.Exit(1)
		}
	}

	if *verbose {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	if *interval <= 0 {
		fmt.Fprintf(os.Stderr, "invalid interval %s: must be positive\n", *interval)
		os.Exit(1)
	}

	if tailcfg.AsServiceName(*service) == "" {
		fmt.Fprintf(os.Stderr, "invalid service name %q: must be like %q\n", *service, "svc:ceph-mgr")
		os.Exit(1)
	}

	if *cephAsok == "" {
		hostname, err := os.Hostname()
		if err != nil {
			slog.Error("failed to get hostname", "error", err)
			os.Exit(1)
		}
		*cephAsok = fmt.Sprintf("/run/ceph/ceph-mgr.%s.asok", hostname)
	}

	var lc local.Client
	if *tsSocket != "" {
		lc.Socket = *tsSocket
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	slog.Info("starting",
		"service", *service,
		"upstream", *upstream,
		"https_port", *httpsPort,
		"ceph_asok", *cephAsok,
		"interval", *interval,
	)

	if err := registerServe(ctx, &lc, *service, *httpsPort, *upstream); err != nil {
		slog.Error("failed to register tailscale serve", "error", err)
		os.Exit(1)
	}

	sdNotify("READY=1\nSTATUS=Polling every " + interval.String())

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	var lastState string
	lastState = poll(ctx, &lc, *cephAsok, *service, lastState)

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down, draining service")
			sdNotify("STOPPING=1")
			drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = setServiceAdvertised(drainCtx, &lc, *service, false)
			drainCancel()
			return
		case <-ticker.C:
			lastState = poll(ctx, &lc, *cephAsok, *service, lastState)
		}
	}
}

func registerServe(ctx context.Context, lc *local.Client, service, httpsPort, upstream string) error {
	sc, err := lc.GetServeConfig(ctx)
	if err != nil {
		return fmt.Errorf("get serve config: %w", err)
	}
	if sc == nil {
		sc = new(ipn.ServeConfig)
	}

	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return fmt.Errorf("get status: %w", err)
	}
	if st.CurrentTailnet == nil {
		return fmt.Errorf("tailscale is not connected to a tailnet (backend state: %s)", st.BackendState)
	}
	mds := st.CurrentTailnet.MagicDNSSuffix

	port, err := strconv.ParseUint(httpsPort, 10, 16)
	if err != nil {
		return fmt.Errorf("invalid https port %q: %w", httpsPort, err)
	}
	if port == 0 {
		return fmt.Errorf("invalid https port %q: must be 1-65535", httpsPort)
	}

	proxyURL, err := ipn.ExpandProxyTargetValue(upstream, []string{"http", "https", "https+insecure"}, "http")
	if err != nil {
		return fmt.Errorf("invalid upstream %q: %w", upstream, err)
	}

	handler := &ipn.HTTPHandler{Proxy: proxyURL}
	sc.SetWebHandler(handler, service, uint16(port), "/", true, mds)

	if err := lc.SetServeConfig(ctx, sc); err != nil {
		return fmt.Errorf("set serve config: %w", err)
	}

	if err := setServiceAdvertised(ctx, lc, service, false); err != nil {
		return fmt.Errorf("initial drain: %w", err)
	}

	slog.Info("registered tailscale serve", "service", service, "port", port, "upstream", proxyURL)
	return nil
}

func setServiceAdvertised(ctx context.Context, lc *local.Client, service string, advertise bool) error {
	prefs, err := lc.GetPrefs(ctx)
	if err != nil {
		return fmt.Errorf("get prefs: %w", err)
	}

	var updated []string
	if advertise {
		if slices.Contains(prefs.AdvertiseServices, service) {
			return nil
		}
		updated = append(prefs.AdvertiseServices, service)
	} else {
		updated = slices.DeleteFunc(slices.Clone(prefs.AdvertiseServices), func(s string) bool { return s == service })
		if len(updated) == len(prefs.AdvertiseServices) {
			return nil
		}
	}

	_, err = lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		AdvertiseServicesSet: true,
		Prefs: ipn.Prefs{
			AdvertiseServices: updated,
		},
	})
	return err
}

func poll(ctx context.Context, lc *local.Client, asokPath, service, lastState string) string {
	state, err := checkMgrStatus(ctx, asokPath)
	if err != nil {
		state = stateError
	}

	if state == lastState {
		return state
	}

	switch state {
	case stateActive:
		slog.Info("mgr is active, advertising", "service", service)
		if err := setServiceAdvertised(ctx, lc, service, true); err != nil {
			slog.Error("failed to advertise, will retry", "error", err)
			return lastState
		}
		sdNotify("STATUS=Active — advertising " + service)

	case stateStandby:
		slog.Info("mgr is standby, draining", "service", service)
		if err := setServiceAdvertised(ctx, lc, service, false); err != nil {
			slog.Error("failed to drain, will retry", "error", err)
			return lastState
		}
		sdNotify("STATUS=Standby — draining " + service)

	case stateError:
		slog.Warn("mgr check failed, draining", "service", service, "error", err)
		if err := setServiceAdvertised(ctx, lc, service, false); err != nil {
			slog.Error("failed to drain, will retry", "error", err)
			return lastState
		}
		sdNotify("STATUS=Error — draining " + service)
	}

	return state
}

func checkMgrStatus(ctx context.Context, asokPath string) (string, error) {
	resp, err := cephAdminCmd(ctx, asokPath, "mgr_status")
	if err != nil {
		return stateError, err
	}

	if json.Valid([]byte(resp)) {
		return stateActive, nil
	}

	if strings.Contains(resp, "unknown command") {
		if _, err := cephAdminCmd(ctx, asokPath, "version"); err != nil {
			return stateError, fmt.Errorf("standby health check: %w", err)
		}
		return stateStandby, nil
	}

	return stateError, fmt.Errorf("mgr_status returned unexpected response: %s", resp)
}

func cephAdminCmd(ctx context.Context, asokPath, command string) (string, error) {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", asokPath)
	if err != nil {
		return "", fmt.Errorf("connect to %s: %w", asokPath, err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return "", fmt.Errorf("set deadline: %w", err)
	}

	cmd := append([]byte(fmt.Sprintf(`{"prefix": "%s"}`, command)), 0)
	slog.Debug("ceph admin request", "socket", asokPath, "command", command)
	if _, err := conn.Write(cmd); err != nil {
		return "", fmt.Errorf("write command: %w", err)
	}

	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return "", fmt.Errorf("read length: %w", err)
	}
	respLen := binary.BigEndian.Uint32(lenBuf[:])

	if respLen == 0 || respLen > 1<<20 {
		return "", fmt.Errorf("unexpected response length: %d", respLen)
	}

	body := make([]byte, respLen)
	if _, err := io.ReadFull(conn, body); err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	slog.Debug("ceph admin response", "socket", asokPath, "command", command, "bytes", respLen, "body", string(body))

	return string(body), nil
}

var sdNotifySocketWarning sync.Once

func sdNotify(state string) {
	slog.Info("sd_notify", "state", state)

	socketAddr := os.Getenv("NOTIFY_SOCKET")
	if socketAddr == "" {
		sdNotifySocketWarning.Do(func() {
			slog.Warn("NOTIFY_SOCKET not set, set Type=notify in the systemd unit to enable status reporting")
		})
		return
	}

	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.Dial("unixgram", socketAddr)
	if err != nil {
		slog.Error("sd_notify dial failed", "error", err)
		return
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte(state)); err != nil {
		slog.Error("sd_notify write failed", "error", err)
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDurationOrDefault(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback, fmt.Errorf("invalid %s=%q: %w", key, v, err)
	}
	return d, nil
}
