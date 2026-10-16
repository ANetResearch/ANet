# Service module deployment example

This directory shows how to offer one capability from a node through the
`service` module: a daemon configuration (`config.json`), an adapter that
does the work (`adapter.py`), and a systemd unit for the adapter
(`anet-service@.service`). The example capability, `image.describe`, reports
the format and dimensions of one image.

## How the parts fit

The daemon receives a call, applies the node's inbound policy and rate
limits, and forwards the call to the adapter over a Unix domain socket with
a bearer token. The adapter answers with JSON. The daemon returns the
result to the caller.

The adapter in this example keeps three properties. Keep them when you
replace it:

1. It listens on the Unix socket only. It opens no network port.
2. It reads the image dimensions from the header and refuses an image above
   the pixel limit (`MAX_PIXELS`) before anything decodes it.
3. It writes no file and logs nothing about a call.

## Installation

1. Create the account and directories:

       useradd --system --home-dir /var/lib/anet-service --shell /usr/sbin/nologin anet-service
       install -d -m 0700 -o anet-service -g anet-service /var/lib/anet-service
       install -d -m 0755 /opt/anet/service-files
       install -m 0644 adapter.py /opt/anet/service-files/adapter.py

2. Create the token shared by the daemon and the adapter:

       head -c 32 /dev/urandom | base64 | install -m 0600 -o anet-service -g anet-service /dev/stdin /var/lib/anet-service/svc.token

3. Install the adapter unit and start it:

       install -m 0644 anet-service@.service /etc/systemd/system/
       systemctl daemon-reload
       systemctl enable --now anet-service@example

4. Copy `config.json` to the daemon's data directory and adjust `name`,
   `summary` and the limits. The daemon runs as the same account
   (`anet-service`); its unit should list `Requires=` and `After=` for the
   adapter unit.

5. Start the daemon and confirm with `anet doctor` that the capability is
   listed.

## Notes

- `inbound.policy` is `closed`: only callers the node has accepted can
  reach the capability. `discard` is set, so the node keeps no copy of the
  input.
- Use `unix://` URLs. An `http://` URL needs `allow_tcp: true` and gives up
  the check that the listener belongs to the expected account.
- The token authenticates the daemon to the adapter. Keep the token file at
  mode 0600.
- Only modules in the default build are used here. A node built without the
  `service` module (`no_service`) cannot serve this capability.
