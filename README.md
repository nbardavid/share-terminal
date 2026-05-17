# control

End-to-end-encrypted CLI terminal sharing via a relay. **tmate-style**: the
host sees its own shell, the client follows in real time and can also type
(TUIs like `nvim` / `htop` / `less` work). 3-word short codes **croc-style**,
mutual authentication via PAKE (SPAKE2), end-to-end XChaCha20-Poly1305
encryption.

The relay never sees plaintext — it's just a rendezvous point.

## Install

```bash
curl -sSfL https://github.com/nbardavid/share-terminal/releases/latest/download/install.sh | sh
```

Installs into `~/.local/bin` (no sudo). If that directory isn't in your
`PATH`, the script tells you the exact line to add to your `.bashrc` /
`.zshrc` / `fish` config.

Supported platforms: Linux / macOS, amd64 / arm64.

## Usage

```bash
control share         # share your terminal read-only, prints a code
control share -w     # same but lets the peer type as well
control join CODE     # join a shared terminal
control update        # update to the latest release
control --version
```

The client targets `wss://relay.nbardavid.dev` by default. To use a different
relay:

```bash
control share --relay wss://my-relay.example.com
# or
export CONTROL_RELAY=wss://my-relay.example.com
```

## How it works

```
[host]                         [relay]                       [client]
  ↕                              │                              ↕
$SHELL ─ PTY ─ encryption ───→ splice bytes ←─── encryption ─ stdin/stdout
                                │
                                ▼
                          sees only ciphertext
```

1. Both peers connect to the relay and present the same 3-word code.
2. The relay pairs them and splices their WebSockets (bidirectional
   `io.Copy`).
3. Over the resulting tunnel, PAKE (SPAKE2) derives a symmetric key on
   both ends. A final HMAC exchange confirms the codes matched.
4. All subsequent frames are encrypted with XChaCha20-Poly1305.
5. The host attaches its terminal to a shared PTY (size =
   `min(host, client)`), fans the PTY output out to both its own stdout
   and the conn. The client receives and renders in raw mode. SIGWINCH
   on either side reconciles the size live.

## Self-hosting the relay

The `control-relay` binary is in every release. On your server:

```bash
# Grab the binary (adapt arch)
curl -fsSL https://github.com/nbardavid/share-terminal/releases/latest/download/control-relay-linux-amd64 \
  -o /usr/local/bin/control-relay
chmod +x /usr/local/bin/control-relay

# Run (default :8080)
control-relay
```

Put it behind your reverse proxy with TLS — make sure the proxy forwards
**WebSocket upgrades** to `/ws`. Verify with
`curl https://your-relay.com/healthz`.

## Security

- **PAKE (SPAKE2)**: a wrong code fails *before* any data is exchanged. The
  relay can't impersonate a peer.
- **AEAD (XChaCha20-Poly1305)**: integrity + confidentiality on every frame.
- **Read-only by default**: the client only receives input rights if the
  host passed `--write`.
- **Explicit host confirmation**: y/N prompt on every peer connection, with
  the peer's `user@host` and the key fingerprint shown.
- **Single-use codes**: expire after 10 min on the relay side.

## Build locally

```bash
git clone https://github.com/nbardavid/share-terminal.git
cd share-terminal
go build ./cmd/control          # or: go install ./cmd/control
go build ./cmd/control-relay
go test ./...
```

## License

To be added.
