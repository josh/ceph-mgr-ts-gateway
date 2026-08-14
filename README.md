# ceph-mgr-ts-gateway

Tailscale service gateway for the Ceph MGR dashboard, following the active MGR.

A Ceph cluster runs several `ceph-mgr` daemons, but only one is active at a time
and only that one serves the dashboard. Run a copy of this gateway on each mgr
host: it polls the local mgr admin socket and advertises the Tailscale service
only while that mgr is the active one, draining on standby or error. The service
VIP follows failover on its own, with no external load balancer or health check.

Each instance registers a Tailscale `serve` config proxying the service's HTTPS
port to the local dashboard, then starts drained — a fresh process never
advertises before its first successful health check.

## Requirements

- A running `tailscaled` already connected to a tailnet.
- The service (`svc:ceph-mgr`) declared in the tailnet policy file. Advertising
  a service the policy file does not grant the node silently does nothing.
- Read access to the mgr admin socket and to the tailscaled local API, which in
  practice means running as root.

## Install

```sh
go install github.com/josh/ceph-mgr-ts-gateway@latest
```

Or clone and `go build .`.

## Usage

```
Usage of ceph-mgr-ts-gateway:
  -ceph-asok string
    	Ceph MGR admin socket path
  -https-port string
    	HTTPS listen port (default "443")
  -interval duration
    	Poll interval (default 30s)
  -service string
    	Tailscale service name (default "svc:ceph-mgr")
  -socket string
    	Tailscale daemon socket path
  -upstream string
    	Local backend address (default "127.0.0.1:8080")
  -verbose
    	Enable debug logging
  -version
    	Print version and exit
```

`-ceph-asok` defaults to `/run/ceph/ceph-mgr.$(hostname).asok`.

A bare `host:port` upstream is expanded to `http://host:port`. The Ceph
dashboard usually listens on HTTPS with a self-signed certificate, so point
`-upstream` at `https+insecure://127.0.0.1:8443` rather than the bare address —
otherwise the proxy speaks plaintext to a TLS port and every request fails.

## Configuration

Each flag falls back to an environment variable. Flags win when both are set.

| Flag          | Environment     | Default                               |
| ------------- | --------------- | ------------------------------------- |
| `-service`    | `TS_SERVICE`    | `svc:ceph-mgr`                        |
| `-upstream`   | `TS_UPSTREAM`   | `127.0.0.1:8080`                      |
| `-https-port` | `TS_HTTPS_PORT` | `443`                                 |
| `-socket`     | `TS_SOCKET`     | tailscaled default                    |
| `-ceph-asok`  | `CEPH_ASOK`     | `/run/ceph/ceph-mgr.$(hostname).asok` |
| `-interval`   | `POLL_INTERVAL` | `30s`                                 |

## systemd

The gateway reports readiness and status over `sd_notify` and drains the service
on `SIGTERM`, so the unit should be `Type=notify`:

```ini
[Unit]
Description=Tailscale service gateway for the Ceph MGR dashboard
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/ceph-mgr-ts-gateway
Environment=TS_UPSTREAM=https+insecure://127.0.0.1:8443
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

`systemctl status` then shows the current state, one of `Polling every 30s`,
`Active — advertising svc:ceph-mgr`, `Standby — draining svc:ceph-mgr`, or
`Error — draining svc:ceph-mgr`.

## License

The project is licensed under the [MIT License](LICENSE).
