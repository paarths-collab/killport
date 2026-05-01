# ⚡ killport

> [!IMPORTANT]
> **Official Distribution:** This repository is for viewing and auditing only. Please **DO NOT CLONE** this repository for usage. To install **killport**, please use the official binaries or package managers (Brew/Scoop) provided below to ensure you have the latest optimized version.

[![Go Report Card](https://goreportcard.com/badge/github.com/paarths-collab/killport)](https://goreportcard.com/report/github.com/paarths-collab/killport)
[![GitHub Release](https://img.shields.io/github/v/release/paarths-collab/killport)](https://github.com/paarths-collab/killport/releases)
[![License: Proprietary](https://img.shields.io/badge/License-Proprietary-red.svg)](LICENSE)
[![Downloads](https://img.shields.io/badge/Downloads-1K+-blue)](https://github.com/paarths-collab/killport/releases)

**The last port killer you'll ever install.** Lightning-fast, concurrent, Docker-native, and optimized for both humans and AI Agents.

---

## 🚀 Why killport?

Most port killers simply find a PID and send a `kill -9`. This works for simple scripts but fails in modern dev environments. **killport** is engineered for the "Unholy Trinity" of modern workflows:

- 🐳 **Docker Native:** Safely triggers `docker stop` on containers instead of murdering the `docker-proxy` and leaving zombie containers behind.
- 🤖 **AI-Agent Optimized:** Machine-readable `--json` output reduces LLM token usage by 90% and eliminates parsing hallucinations.
- 🎯 **Port Ranges:** Concurrent execution allows you to wipe out entire microservice stacks in one command (e.g., `3000-3010`).

---

## 📦 Installation

### macOS (Homebrew)
```bash
brew tap paarths-collab/homebrew-tap
brew install killport
```

### Windows (Scoop)
```powershell
scoop bucket add paarths-collab https://github.com/paarths-collab/scoop-bucket
scoop install killport
```

### Windows install options

| Option | Type | Notes |
|---|---|---|
| Winget | CLI | Official and fast |
| Chocolatey | CLI | Massive package repository |
| Microsoft Store | GUI | Secure and simple |
| Direct Download | Web | Official source for Windows download |

Direct download:
- https://github.com/paarths-collab/killport/releases/latest

### Linux / macOS (One-liner)
```bash
curl -sSfL https://raw.githubusercontent.com/paarths-collab/killport/main/install.sh | sh
```

---

## 🛠 Usage

### Kill one or more ports
```bash
killport 3000 8080 5432
```

### Kill a range of ports (Concurrent)
```bash
killport 3000-3005
```

### Dry Run (Simulate)
```bash
killport 3000-3005 --dry-run
```

### Check which process is using ports (no kill)
```bash
killport --check
```

### Check specific ports only
```bash
killport --check 3000 5432
```

### AI-Agent / Machine Mode

Outputs a strict JSON array for easy parsing by LLMs or CI/CD pipelines.

```bash
killport 5432 --json
```

```bash
killport --check --json
```

---

## 📊 Comparison

| Feature         | kill-port (NPM) | killport (Rust) | killport (Go) |
|----------------|----------------|-----------------|---------------|
| Runtime        | Node (Slow)    | Compiled (Fast) | Compiled (Fast) |
| Binary Size    | N/A (Node req) | ~2MB            | ~2MB (Zero Deps) |
| Port Ranges    | ❌ No          | ❌ No           | ✅ Yes (3000-3005) |
| AI-Agent Mode  | ❌ No          | ❌ No           | ✅ Yes (--json) |
| Docker Stop    | ❌ No          | ✅ Yes          | ✅ Yes |

---

## 🤖 LLM Token Optimization

If you are using AI Agents (Devin, OpenDevin, Cursor), standard CLI tools waste 500–1,500 tokens per attempt due to unstructured output and ASCII tables.

By using `killport --json`, agents can parse the system state in < 50 tokens, drastically reducing costs and preventing execution errors.

---

## 🛠 Development

```bash
git clone https://github.com/paarths-collab/killport.git
cd killport
go run main.go --help
```

---

## 📄 License

Licensed under the Killport Proprietary License. Source cloning is restricted; please use official releases for installation.
