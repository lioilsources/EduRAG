# FIX: devcontainer nefungoval — přehled oprav

## Problém 1: lunaroute-proxy okamžitě crashoval (GLIBC mismatch)

**Soubor:** `.devcontainer/Dockerfile.lunaroute`

**Symptom:** Kontejner `lunaroute-proxy` exitoval hned po startu s kódem 1.
Logy obsahovaly tisíce řádků:
```
lunaroute-server: /lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.38' not found
lunaroute-server: /lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.39' not found
```
Kvůli `restart: unless-stopped` se kontejner neustále restartoval a generoval 230 KB logů.

**Příčina:** Builder image `rust:1.94-slim` je postaven na Debian Trixie (GLIBC 2.39).
Runtime image `debian:bookworm-slim` má pouze GLIBC 2.36 — binárka byla nekompatibilní.

**Oprava:**
```diff
- FROM debian:bookworm-slim
+ FROM debian:trixie-slim
```

---

## Problém 2: dev kontejner okamžitě exitoval

**Soubor:** `.devcontainer/docker-compose.yml`

**Symptom:** VS Code se nemohl připojit k `go-dev-container`:
```
Shell server terminated (code: 1, signal: null)
Error: can only create exec sessions on running containers: container state improper
```

**Příčina:** Služba `dev` neměla definovaný `command`. Base image `devcontainers/go:1.25`
spouští `bash`, ale bez připojeného terminálu bash okamžitě skončí — kontejner exitoval
dřív, než se k němu VS Code stihl připojit.

**Oprava:**
```diff
+ command: sleep infinity
```

---

## Problém 3: soubory z EduRAG nebyly v kontejneru viditelné

**Soubor:** `.devcontainer/docker-compose.yml`

**Symptom:** `ls /workspace` v kontejneru vracel `Permission denied`, přestože
volume mount byl správný (`..:/workspace`) a UID hostitele (1000) odpovídal UID
uživatele `vscode` v kontejneru (1000).

**Příčina:** SELinux. Hostitelské soubory mají label `user_home_t`,
kontejner běží v doméně `container_t`. SELinux zamítl přístup:
```
system_u:system_r:container_t:s0:c284,c704  →  user_home_t  = DENIED
```

**Oprava:** Přidat příznak `:z` k volume mountům — Docker přeoznačí soubory
na `container_file_t`, které je pro `container_t` přístupné:
```diff
- - ..:/workspace:cached
- - ./continue-config/config.yaml:/home/vscode/.continue/config.yaml:ro
- - ./continue-config/.env:/home/vscode/.continue/.env:ro
+ - ..:/workspace:cached,z
+ - ./continue-config/config.yaml:/home/vscode/.continue/config.yaml:ro,z
+ - ./continue-config/.env:/home/vscode/.continue/.env:ro,z
```

---

## Problém 4: VS Code nemohl spustit kontejnery — konflikt názvů

**Soubor:** `.devcontainer/docker-compose.yml`

**Symptom:** Při každém "Reopen in Container" VS Code hlásil:
```
Error response from daemon: container create: creating container storage:
the container name "lunaroute-proxy" is already in use
```

**Příčina:** `container_name` byl napevno nastaven na `lunaroute-proxy` a `go-dev-container`.
VS Code spouští docker-compose s projektem `edurag_devcontainer` a příznakem
`--remove-existing-container` — ale ten odstraní pouze kontejnery které sám vytvořil
(identifikuje je pomocí vlastních labels). Kontejnery spuštěné ručně (jiný projekt)
neodstraní, ale jejich jméno je obsazené → konflikt.

**Oprava:** Odebrat `container_name` z obou služeb. Docker Compose pak generuje
unikátní jména podle project name (`edurag_devcontainer-lunaroute-1` atd.),
takže ruční spuštění a VS Code se navzájem neblokují:
```diff
  lunaroute:
-   container_name: lunaroute-proxy

  dev:
-   container_name: go-dev-container
```
