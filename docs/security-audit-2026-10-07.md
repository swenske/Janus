# Revue de sécurité défensive — configuration, secrets, durcissement, CI

Date : 2026-10-07
Périmètre : mauvaises pratiques et faiblesses de configuration sur tout le
dépôt — gestion des secrets, permissions, durcissement (noyau, SELinux,
conteneurs, unités systemd), dépendances, pipelines CI, exposition réseau.
Ne reprend pas les points corrigés par l'audit du 2026-09-30
(`security-audit-2026-09-30.md`) ; ses deux constats laissés ouverts (#15
rootfs non reproductible, #16 bundle incohérent) le restent.
Méthode : `govulncheck` sur les deux modules Go, `npm audit` sur les trois
frontends, lecture des 7 workflows, des 19 Dockerfiles, des scripts de
déploiement, des configs noyau, de la politique SELinux et des serveurs
HTTP du Controller. Pas de code d'exploitation : chaque entrée donne
l'emplacement, le risque, la gravité et le correctif.

## Suivi des corrections

Mis à jour le 2026-10-07, première passe (« groupe A » : les corrections
sans effet fonctionnel). Corrigés : #2, #3, #4, #7, #8 (partie
interpolation), #9 (partie Compose et `EXPOSE`), #13, #14, #15, #16, en
commits séparés par thème (`ci:`, `controller:`, `lint:`, `site:`,
`docs:`). Chaque section concernée porte un paragraphe **Corrigé** qui
décrit la correction et sa vérification.

Deuxième passe le même jour (« groupe B » : les corrections qui changent
un comportement, chacune prouvée par son test QEMU) : #5, #6, #10, #12,
et #8 (l'étape apt épingle la clé d'hôte dès que la variable de dépôt
`APT_HOST_KEY` existe - à créer, voir la section). Restent ouverts : #1,
#9 (`USER` dans l'image : migration du propriétaire du volume), #11 -
les décisions du groupe C.

## Tableau de synthèse

| # | Gravité | Statut | Thème | Emplacement | Titre |
|---|---------|--------|-------|-------------|-------|
| 1 | Élevée | Ouvert | Durcissement noyau | `kernel/configs/janus_{stable,longterm}_defconfig` | Noyau x86 compilé sans aucune mitigation CPU (`CPU_MITIGATIONS`) |
| 2 | Moyenne | Corrigé | CI | `.github/workflows/claude-review.yml`, `claude-issue-triage.yml` | Workflows Claude déclenchables par n'importe quel commentaire |
| 3 | Moyenne | Corrigé | Durcissement HTTP | `dashboard/backend/main.go` | Aucun en-tête de sécurité HTTP sur le Controller |
| 4 | Moyenne | Corrigé | Exposition réseau | `dashboard/backend/main.go`, `register.go` | Serveurs HTTP sans délais ni version TLS minimale explicite |
| 5 | Moyenne | Corrigé | Exposition réseau | `dashboard/backend/register.go`, `internal/pending` | Enregistrement non authentifié sans débit ni plafond |
| 6 | Moyenne | Corrigé | Durcissement nœud | `internal/haproxy/manager.go` | Section `global` d'une config HAProxy appliquée non contrainte |
| 7 | Faible | Corrigé | CI | `.github/workflows/ci.yml` | Pas de bloc `permissions` |
| 8 | Faible | Corrigé | CI | `.github/workflows/image-build.yml` | Entrée de dispatch interpolée dans un `run:` ; TOFU SSH à la publication apt |
| 9 | Faible | Partiel | Conteneur | `dashboard/Dockerfile`, `examples/compose/compose.yaml` | Controller en root, réseau hôte, Compose sans durcissement |
| 10 | Faible | Corrigé | Exposition réseau | `internal/exporter/exporter.go` | Exporter actif par défaut, HTTP clair, toutes interfaces |
| 11 | Faible | Ouvert | SELinux | `selinux/policy.conf` | `janusd_t` a toutes les capacités |
| 12 | Faible | Corrigé | RBAC | `internal/rbac/rbac.go` | Configs BGP et VRRP (mots de passe) lisibles par `os:reader` |
| 13 | Faible | Corrigé | Outillage | `.golangci.yml` | Pas de linter sécurité (`gosec`) |
| 14 | Faible | Corrigé | Dépendances | `site/docs/package.json` | Dépendances de build du site docs avec vulnérabilités connues |
| 15 | Faible | Corrigé | Robustesse | `dashboard/backend/internal/nodeproxy/ops.go` | `decodeJSON` sans limite de taille |
| 16 | Faible | Corrigé | Durcissement | `site/deploy/janus-site.service` | Quelques verrous systemd manquants |

## 1. (Élevée) Noyau x86 compilé sans aucune mitigation CPU

- **Où** : `kernel/configs/janus_stable_defconfig:709` et
  `kernel/configs/janus_longterm_defconfig:690` :
  `# CONFIG_CPU_MITIGATIONS is not set`. Aucune sous-option
  `CONFIG_MITIGATION_*` n'existe dans ces configs. La config arm64
  (`janus_rpi4_defconfig:856`) l'a bien.
- **Risque** : PTI, retpoline, les flush MDS/L1TF et les autres
  mitigations Spectre/Meltdown sont absents du binaire ; `mitigations=auto`
  sur la ligne de commande ne peut rien activer. Pour un équilibreur qui
  termine du TLS sur des hyperviseurs partagés, c'est le premier point à
  régler.
- **Correctif** (jamais à la main, via le flux kbuild) :

  ```sh
  make kernel-menuconfig KERNEL_TRACK=stable
  #   Processor type and features › Mitigations for CPU vulnerabilities = y
  #   (sous-options par défaut)
  make kernel-config-refresh KERNEL_TRACK=stable
  make kernel-menuconfig KERNEL_TRACK=longterm
  make kernel-config-refresh KERNEL_TRACK=longterm
  make kernel-built-files kernel-built-files-longterm
  grep -E 'CONFIG_(CPU_MITIGATIONS|MITIGATION_PAGE_TABLE_ISOLATION|MITIGATION_RETPOLINE|MITIGATION_SPECTRE_V2|MITIGATION_MDS)=' build/kernel/*/.config
  go test ./hack/kconfig && make qemu-hardening-test
  ```

- **Dans la même passe** : `CONFIG_VMAP_STACK` (stable:1000,
  longterm:980) est désactivé alors que KSPP le demande ;
  `CONFIG_IO_URING=y` (stable:493, longterm:480, rpi4:487) n'est utilisé
  ni par Go ni par cette build d'HAProxy. Activer le premier, désactiver
  le second.

## 2. (Moyenne) Workflows Claude déclenchables par n'importe quel commentaire

- **Où** : `.github/workflows/claude-review.yml:20-23` et
  `claude-issue-triage.yml:17-20` : la seule condition est
  `contains(body, '@claude')`.
- **Risque** : tout compte GitHub peut consommer la clé
  `ANTHROPIC_API_KEY` et faire poster des commentaires avec un jeton
  `issues: write` / `pull-requests: write`, le contenu de la PR tierce
  servant d'entrée au modèle.
- **Correctif** :

  ```diff
       if: >
         contains(github.event.comment.body, '@claude') &&
         github.event.comment.user.login != 'claude[bot]' &&
  +      contains(fromJSON('["OWNER","MEMBER","COLLABORATOR"]'), github.event.comment.author_association) &&
         github.event.issue.pull_request != null
  ```

  Retirer aussi `id-token: write` des deux fichiers si l'app GitHub n'est
  pas utilisée (authentification par clé API).
- **Corrigé** : les deux `if:` exigent
  `github.event.comment.author_association` dans OWNER/MEMBER/COLLABORATOR.
  `id-token: write` reste : l'app GitHub Claude est installée et l'action
  s'en sert pour le jeton de l'app. YAML validé.

## 3. (Moyenne) Aucun en-tête de sécurité HTTP sur le Controller

- **Où** : `dashboard/backend/main.go:230`. Le site
  (`site/backend/main.go:136-139`) envoie nosniff, Referrer-Policy,
  X-Frame-Options et une CSP ; le Controller, qui porte les sessions et
  les pages de nœud, n'envoie rien.
- **Risque** : pas de clickjacking possible grâce à SameSite=Strict, mais
  aucune défense en profondeur contre un XSS dans la SPA ou une page de
  nœud.
- **Correctif** :

  ```go
  // dashboard/backend/main.go
  func securityHeaders(next http.Handler) http.Handler {
  	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
  		h := w.Header()
  		h.Set("X-Content-Type-Options", "nosniff")
  		h.Set("X-Frame-Options", "DENY")
  		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
  		h.Set("Strict-Transport-Security", "max-age=31536000")
  		h.Set("Content-Security-Policy-Report-Only",
  			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
  		next.ServeHTTP(w, r)
  	})
  }
  ```

  et `Handler: app.unlessRestored(securityHeaders(http.NewCrossOriginProtection().Handler(...)))`.
  Valider la CSP en Report-Only avec `make qemu-dashboard-test` (zéro
  erreur console, les deux thèmes) avant de la passer en
  `Content-Security-Policy`.
- **Corrigé** : `dashboard/backend/security_headers.go`, posé autour de
  tout le port principal (page, API, pages de nœud). CSP en
  `Content-Security-Policy-Report-Only` pour l'instant (`script-src
  'self'`, `style-src 'self' 'unsafe-inline'` pour les styles inline de
  React, `img-src 'self' data: blob:` pour le QR TOTP et les
  téléchargements, `frame-ancestors 'none'`) ; à passer en mode bloquant
  quand `qemu-dashboard-test` reste sans violation dans la console.
  Vérifié sur un `dashboardd` lancé localement : les cinq en-têtes sur `/`
  et sur `/api/auth/status`.

## 4. (Moyenne) Serveurs HTTP sans délais ni version TLS minimale explicite

- **Où** : `dashboard/backend/main.go:223-231` (port principal) et
  `register.go:62` (port :8443, sans certificat client, joignable par
  tout nœud à provisionner). Le site et l'exporter ont déjà leurs délais.
- **Risque** : aucun `ReadHeaderTimeout`/`IdleTimeout`, donc des
  connexions lentes ou oisives retiennent des descripteurs sans limite.
- **Correctif** :

  ```diff
   	srv := &http.Server{
   		Addr:      *addr,
  +		ReadHeaderTimeout: 10 * time.Second,
  +		IdleTimeout:       2 * time.Minute, // pas de WriteTimeout : les flux SSE
  +		MaxHeaderBytes:    64 << 10,
   		Handler:   ...,
  -		TLSConfig: &tls.Config{GetCertificate: uiCert.GetCertificate},
  +		TLSConfig: &tls.Config{GetCertificate: uiCert.GetCertificate, MinVersion: tls.VersionTLS12},
   	}
  ```

  ```diff
  -	srv := &http.Server{Handler: mux}
  +	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
  ```
- **Corrigé** : port principal `ReadHeaderTimeout` 10 s, `IdleTimeout`
  2 min, `MaxHeaderBytes` 64 KiB, `MinVersion` TLS 1.2 (pas de
  `WriteTimeout` : SSE, consoles, captures) ; port d'enregistrement
  10 s / 30 s / 30 s / 1 min / 64 KiB. Vérifié localement : une
  connexion qui n'envoie jamais la fin de sa ligne de requête est fermée
  après 10 s.

## 5. (Moyenne) Enregistrement non authentifié sans débit ni plafond

- **Où** : `dashboard/backend/register.go:103-110` accepte 1 MiB par
  requête ; `internal/pending/pending.go:156` persiste chaque annonce sur
  disque sans limite.
- **Risque** : qui atteint :8443 peut remplir le disque du Controller et
  noyer la file d'approbation. Le limiteur de login
  (`internal/auth/limiter.go`) existe déjà et peut servir de modèle.
- **Correctif** :

  ```go
  // register.go, en tête de handleRegister
  if ok, wait := a.registerLimiter.Allow(clientAddr(r)); !ok { // un limiteur dédié, ~10/min/IP
  	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
  	http.Error(w, "too many registrations from this address", http.StatusTooManyRequests)
  	return
  }
  if a.pending.Len() >= maxPending { // 200, par exemple
  	http.Error(w, "the approval queue is full", http.StatusServiceUnavailable)
  	return
  }
  ```

- **Corrigé** : `dashboard/backend/internal/ratelimit` (seau à jetons par
  adresse, 10 000 adresses au plus), posé en tête de `handleRegister` :
  10 annonces d'un coup par adresse puis une toutes les 6 s, refus 429
  avec `Retry-After` (le nœud réessaie seul, 5 s doublant jusqu'à
  2 min). File d'attente plafonnée à 200 annonces (`Waiting()` du store),
  refus 503 au-delà ; un nœud admis sur un jeton (machine, enrôlement)
  ne passe pas par la file et n'est donc jamais bloqué. Corps limité à
  256 KiB au lieu de 1 MiB. `TestRegisterBounded` (429 puis 503, jeton
  accepté file pleine), `make qemu-self-register-test` (un vrai nœud
  s'enregistre toujours). Documenté dans `dashboard/README.md`.

## 6. (Moyenne) Section `global` d'une config HAProxy appliquée non contrainte

- **Où** : `internal/haproxy/manager.go:165` (`Validate`) et `:205`
  (`Apply`) ne vérifient que `haproxy -c`. `docs/haproxy-config.md:18-26`
  documente `chroot`, `uid`/`gid` et le `stats socket` de janusd comme
  obligatoires, mais rien ne l'impose.
- **Risque** : un `os:operator` peut retirer le drop de privilèges,
  ajouter `stats socket ipv4@0.0.0.0:9999 level admin` (administration
  d'HAProxy sans authentification depuis le réseau), `external-check`,
  `insecure-fork-wanted` ou `set-dumpable`. SELinux (`haproxy_t`) borne
  les dégâts, mais ce sont des garanties du produit qui doivent être
  vérifiées avant l'écriture.
- **Correctif** :

  ```go
  // internal/haproxy/globalcheck.go
  var forbiddenGlobal = regexp.MustCompile(`^\s*(master-worker|daemon|external-check|insecure-fork-wanted|set-dumpable|setenv|presetenv|resetenv|unsetenv|lua-load|lua-load-per-thread|expose-deprecated-directives)\b`)

  func CheckGlobal(cfg []byte) error {
  	want := map[string]bool{"chroot /var/empty": false, "uid 1000": false, "gid 1000": false,
  		"stats socket /run/janus/haproxy-admin.sock mode 660 level admin": false}
  	inGlobal := false
  	for _, raw := range strings.Split(string(cfg), "\n") {
  		line := strings.TrimSpace(strings.SplitN(raw, "#", 2)[0])
  		if line == "" { continue }
  		if !strings.HasPrefix(raw, " ") && !strings.HasPrefix(raw, "\t") {
  			if line == "program" || strings.HasPrefix(line, "program ") { return errors.New("program sections are not allowed") }
  			inGlobal = line == "global"
  			continue
  		}
  		if !inGlobal { continue }
  		if forbiddenGlobal.MatchString(line) { return fmt.Errorf("global: %q is not allowed on a Janus node", line) }
  		if strings.HasPrefix(line, "stats socket") && line != "stats socket /run/janus/haproxy-admin.sock mode 660 level admin" {
  			return fmt.Errorf("global: only janusd's stats socket is allowed, got %q", line)
  		}
  		if _, ok := want[line]; ok { want[line] = true }
  	}
  	for k, seen := range want { if !seen { return fmt.Errorf("global: %q is required", k) } }
  	return nil
  }
  ```

  À appeler au début de `Validate` et d'`Apply` ; `haproxyError` le rend
  en `InvalidArgument`. Les scripts `hack/qemu-*` et les `examples/` qui
  appliquent des configs sans `global` complet devront l'avoir.

- **Corrigé** : `internal/haproxy/globalcheck.go` (`GlobalPolicy`,
  `NodePolicy`), vérifié par `Validate` avant `haproxy -c` - donc par
  `Apply` et `ValidateConfig`, jamais au boot (la config sur STATE reste
  celle du nœud). Requis exactement : `chroot <-haproxy-chroot-dir>`,
  `uid 1000`, `gid 1000`, `stats socket <-haproxy-stats-socket> mode 660
  level admin` (paramètres dans n'importe quel ordre, rien d'autre) ;
  refusés : tout autre `stats socket`, `daemon`, `master-worker`,
  `external-check`, `insecure-fork-wanted`, `set-dumpable`,
  `setenv`/`presetenv`/`resetenv`/`unsetenv`, toute section `program`.
  Les sections sont reconnues par mot-clé, pas par indentation
  (HAProxy n'en tient pas compte) ; les commentaires sont ignorés.
  janusd ne pose la politique qu'avec `-manage-host` (un vrai nœud) :
  sur un hôte ou en CI, la config du test reste libre. Vérifié que la
  config de production (lgslbpub01), le bootstrap et `examples/haproxy/
  web.cfg` passent tels quels (`TestGlobalPolicyAcceptsTheNodeConfigs`),
  `TestGlobalPolicy` (22 cas), `make qemu-system-api-test` (sans `uid`,
  socket réseau, `daemon` : refusés, la config courante reste servie).
  Documenté dans `docs/haproxy-config.md`.

## 7. (Faible) `ci.yml` sans bloc `permissions`

- **Où** : `.github/workflows/ci.yml:13`. Tous les autres workflows le
  fixent.
- **Risque** : le jeton hérite du réglage par défaut du dépôt sur les push
  vers main.
- **Correctif** :

  ```diff
   on:
     push:
       branches: [main]
     pull_request:
  +
  +permissions:
  +  contents: read
  ```
- **Corrigé** : `permissions: contents: read` au niveau du workflow.

## 8. (Faible) Entrée de dispatch interpolée dans un `run:` ; TOFU SSH à la publication apt

- **Où** : `.github/workflows/image-build.yml:1255-1262` injecte
  `${{ inputs.release_version }}` dans le shell alors que les autres
  étapes passent par `env: RELEASE_VERSION`. Même fichier `:1426-1429` :
  `StrictHostKeyChecking=accept-new` vers `apt.int.sw-servers.net`, alors
  que `site-deploy.yml:84-85` épingle la clé d'hôte.
- **Risque** : injection de shell par une entrée (limitée aux
  collaborateurs qui dispatchent) ; premier contact SSH non vérifié.
- **Correctif** :

  ```diff
  -          if [ -n "${{ inputs.release_version }}" ]; then
  -            docker tag janus-controller swenske/janus-controller:${{ inputs.release_version }}
  +        env:
  +          RELEASE_VERSION: ${{ inputs.release_version }}
  +        run: |
  +          if [ -n "$RELEASE_VERSION" ]; then
  +            docker tag janus-controller "swenske/janus-controller:$RELEASE_VERSION"
  ```

  ```diff
  -            ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -i "$key" "$host" \
  +            printf '%s\n' "$APT_HOST_KEY" > "$HOME/.ssh/known_hosts_janus_apt"   # variable de dépôt, comme SITE_HOST_KEY
  +            ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$HOME/.ssh/known_hosts_janus_apt" -i "$key" "$host" \
  ```
- **Corrigé (interpolation)** : l'étape Docker Hub et l'envoi du dispatch
  `site-deploy` lisent `RELEASE_VERSION` et `COMMIT_SHA` dans `env:`.
- **Corrigé (clé d'hôte)** : l'étape lit la variable de dépôt
  `APT_HOST_KEY` ; si elle existe, `StrictHostKeyChecking=yes` avec un
  `known_hosts` temporaire qui ne contient qu'elle ; sinon, l'ancien
  `accept-new` et un `::warning::` sur le run - la release ne casse
  pas tant que la variable manque. La clé, lue depuis deux points du
  réseau (ce poste et `janus-runner01`), identique :

  ```sh
  gh variable set APT_HOST_KEY --body "apt.int.sw-servers.net ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBQunxx8M6pXh3XMh8NR68e//+/S+mtwa9wzJgLZiNkQ"
  ```

## 9. (Faible) Controller en root, réseau hôte, Compose sans durcissement

- **Où** : `dashboard/Dockerfile` (aucun `USER`, image `FROM scratch`) et
  `examples/compose/compose.yaml:7-9`. Le `EXPOSE 8080 8443 9500-9599` à
  la ligne 78 décrit un pool de ports qui n'existe plus.
- **Risque** : avec `network_mode: host`, un root dans le conteneur a les
  capacités réseau de l'hôte.
- **Correctif** :

  ```diff
   FROM scratch AS export
  +COPY --from=build --chown=65532:65532 /out/empty /data
   COPY --from=build /out/dashboardd /dashboardd
   ...
  -EXPOSE 8080 8443 9500-9599
  +EXPOSE 8080 8443
  +USER 65532:65532
   ENTRYPOINT ["/dashboardd"]
  ```

  ```diff
     janus-controller:
       network_mode: host
  +    read_only: true
  +    tmpfs: [/tmp]
  +    cap_drop: [ALL]
  +    security_opt: [no-new-privileges:true]
  ```

  Un `-addr :443` demandera alors `cap_add: [NET_BIND_SERVICE]` (à dire
  dans `dashboard/README.md`). L'updater garde `docker.sock` par
  conception ; lui donner au moins `cap_drop`/`no-new-privileges`.
- **Corrigé (Compose et `EXPOSE`)** : le service Controller a
  `read_only: true`, `cap_drop: [ALL]`, `security_opt:
  [no-new-privileges:true]` (dashboardd n'écrit que sous `/data` et dans
  le socket de l'updater : vérifié par lecture de tous les
  `os.WriteFile`/`OpenFile`/`MkdirAll` du backend) ; l'updater a
  `cap_drop` et `no-new-privileges` (pas `read_only` : `docker compose`
  écrit son propre état). `docker compose config` passe, le bloc embarqué
  dans `dashboard/README.md` est régénéré (`make docs-examples`). Reste
  `USER 65532` dans l'image : il change le propriétaire de `/data` des
  installations existantes, à faire avec une note de release.

## 10. (Faible) Exporter actif par défaut, HTTP clair, toutes interfaces

- **Où** : `internal/exporter/exporter.go:52` (`Enabled: true`) et `:239`
  (`net.Listen("tcp", ":port")`). Documenté dans `docs/metrics.md:16-35`.
- **Risque** : un nœud fraîchement installé sans l'extension nftables
  expose ses métriques (noms de backends, versions, compteurs d'erreurs
  d'auth) à tout le réseau.
- **Correctif** : une adresse d'écoute dans la config, exposée dans le
  proto, `janusctl system exporter` et la page Metrics ; même remarque
  pour node_exporter si son réglage n'a pas d'adresse.

  ```diff
   type Config struct {
   	Enabled bool   `json:"enabled"`
   	Port    uint32 `json:"port"`
  +	Address string `json:"address"` // "" = toutes les interfaces ; "127.0.0.1" ou l'IP de management
   }
  -	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
  +	lis, err := net.Listen("tcp", net.JoinHostPort(cfg.Address, strconv.Itoa(int(cfg.Port))))
  ```

- **Corrigé** : `MetricsConfig.address` (proto, champ 3 ; vide = toutes
  les adresses, une IP du nœud sinon, refusée autrement), portée par
  `internal/exporter.Config`, `janusctl system metrics -address IP|'*'`
  (même convention que `node-exporter`), le relais et la page **Apps ›
  Janus exporter** (champ Address, rappel qu'une adresse de management
  garde les métriques hors des réseaux servis). La valeur par défaut
  reste « toutes les adresses » : la changer aurait cassé les scrapes
  existants. `make qemu-metrics-test` (sur 127.0.0.1 l'hôte ne joint
  plus le port, `'*'` le remet, `not-an-ip` refusé). Documenté dans
  `docs/metrics.md` ; `janusctl-reference.md` régénéré.

## 11. (Faible) `janusd_t` a toutes les capacités

- **Où** : `selinux/policy.conf:287-288` :
  `allow janusd_t self:capability *; allow janusd_t self:capability2 *;`.
- **Risque** : c'est l'exception à la règle « chaque règle vient d'un
  refus réel » ; la politique ne borne pas le démon de contrôle.
- **Correctif** : remplacer par un ensemble vide, lancer
  `make qemu-selinux-test` plus les tests enforcing des fonctions
  (install, upgrade, pcap, sysctl, network apply, firewall, vrrp, bgp), et
  coller les capacités refusées (`printk_ratelimit=0`). Attendu :
  `net_admin net_raw net_bind_service sys_admin sys_boot sys_time
  sys_resource setuid setgid chown dac_override dac_read_search fowner
  fsetid kill audit_write` ; la liste réelle fait foi.

## 12. (Faible) Configs BGP et VRRP lisibles par `os:reader`

- **Où** : `internal/rbac/rbac.go:117` (`BGPGetConfig: readers`) et `:120`
  (`VRRPGetConfig: readers`), alors que `docs/bgp.md:27` montre
  `password "secret"` (TCP MD5) et keepalived porte `auth_pass`. Consul
  est déjà `adminOnly` pour cette raison (`:123`).
- **Risque** : un lecteur obtient les secrets de session BGP/VRRP.
- **Correctif** :

  ```diff
  -	"/janus.v1alpha1.NetworkService/BGPGetConfig":         readers,
  +	"/janus.v1alpha1.NetworkService/BGPGetConfig":         operators, // password "..." TCP MD5
  -	"/janus.v1alpha1.NetworkService/VRRPGetConfig":        readers,
  +	"/janus.v1alpha1.NetworkService/VRRPGetConfig":        operators, // auth_pass
  ```

  Alternative : garder `readers` et masquer `password`/`auth_pass` dans la
  réponse quand l'appelant n'est pas opérateur. Mettre
  `docs/private-cloud/api-reference.md` à jour (`-update`).

- **Corrigé** : les deux RPC en `operators` ; `api-reference.md`
  régénéré ; `docs/bgp.md` et `docs/vrrp.md` le disent. Les pages BGP et
  VRRP du Controller n'offrent l'éditeur qu'à qui `may()` l'autorise
  (`ModuleConfigEditor`, `readMethod`) : un lecteur voit l'état, pas la
  configuration.

## 13. (Faible) Pas de linter sécurité

- **Où** : `.golangci.yml:6-13` : six linters, aucun orienté sécurité,
  alors que le code porte déjà des `//nolint:gosec` sans effet.
- **Correctif** :

  ```diff
     enable:
       - govet
       - staticcheck
       - errcheck
       - ineffassign
       - unused
       - misspell
  +    - gosec
  +  settings:
  +    gosec:
  +      excludes: [G204, G304] # exec/open sur des chemins voulus (hack/, lifecycle) ; à réévaluer
  ```
- **Corrigé** : `gosec` activé sur les deux modules, pas sur les tests,
  sans G101/G115/G122/G204/G304/G306 ni les analyses de teinte
  (G702-G706), chacune justifiée dans `.golangci.yml`. Les 11 remontées
  restantes sont des faux positifs annotés `//nolint:gosec // Gnnn: ...`
  à leur ligne (clé sur le stdout de janus-acme, `cancel` gardé pour plus
  tard, vérification d'empreinte sans cache de session, pid de /proc,
  gigue de renouvellement ACME). Les règles Slowloris, TLS, crypto faible
  restent actives. `golangci-lint run` : 0 problème sur les deux modules.

## 14. (Faible) Dépendances de build du site docs avec vulnérabilités connues

- **Où** : `site/docs/package.json` : `braces` (élevée, DoS par motif
  imbriqué, aucune version corrigée publiée), `postcss-selector-parser`
  < 7.1.6, `smol-toml` ≤ 1.8.0, `katex` ≤ 0.18.1, tous tirés par
  Starlight.
- **Risque** : faible : elles ne tournent qu'au build dans Docker, jamais
  sur le site servi.
- **Correctif** :

  ```json
  "overrides": { "postcss-selector-parser": ">=7.1.6", "smol-toml": ">=1.8.1", "katex": ">=0.18.2" }
  ```

  puis `make docs-build docs-smoke`. Pour `braces`, attendre le correctif
  amont ; Dependabot hebdomadaire couvre déjà ce répertoire.
- **Corrigé** : `overrides` pour `postcss-selector-parser`, `smol-toml`
  et `katex` dans `site/docs/package.json`, lockfile refait dans l'image
  Node épinglée des docs. `npm audit` ne signale plus que `braces` (aucune
  version corrigée). `make docs-build` passe.

## 15. (Faible) `decodeJSON` du relais sans limite de taille

- **Où** : `dashboard/backend/internal/nodeproxy/ops.go:264-270` lit le
  corps entier.
- **Risque** : les routes sont authentifiées, mais un opérateur peut faire
  allouer des Go au Controller.
- **Correctif** :

  ```diff
   func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
  -	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
  +	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(v); err != nil { // configs HAProxy + fichiers (1 MiB) + marge
  ```
- **Corrigé** : `http.MaxBytesReader` à 4 MiB (`maxJSONBody`) dans
  `decodeJSON`. `go test ./dashboard/...` passe.

## 16. (Faible) Unit systemd du site : quelques verrous manquent

- **Où** : `site/deploy/janus-site.service`, déjà bien durci.
- **Correctif** : ajouter après `ReadWritePaths=` :

  ```ini
  RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
  RestrictNamespaces=yes
  RestrictRealtime=yes
  ProtectClock=yes
  ProtectHostname=yes
  ProtectProc=invisible
  SystemCallArchitectures=native
  SystemCallFilter=@system-service
  SystemCallFilter=~@privileged @resources
  ```

  Valider sur le CT avec `systemd-analyze security janus-site` ; rien ne
  change sur le CT sans validation.
- **Corrigé (dans le dépôt)** : les neuf directives ajoutées à
  `site/deploy/janus-site.service`. Elles ne s'appliquent au CT 222 qu'au
  prochain `setup-ct.sh`, après validation ; `systemd-analyze security
  janus-site` à relire à ce moment-là.

## Points vérifiés et jugés corrects (pour éviter de les re-signaler)

- mTLS TLS 1.3 partout côté nœud (`internal/pki/tls.go`) ; cookies
  `HttpOnly`/`Secure`/`SameSite=Strict` et `CrossOriginProtection` sur le
  Controller.
- Limiteur de login ; jetons de session, API et d'enrôlement de 256 bits,
  stockés hachés (`internal/auth`, `internal/enroll`).
- Clé maître hors du volume de données avec avertissement sinon
  (`internal/secrets`, `main.go:161`).
- `Read`/`Copy` réservés à admin avec `SecretPaths` plus détection PEM
  (`internal/api/secretfiles.go`) ; `ConsulGetConfig` admin.
- Updater borné au dépôt `swenske/janus-controller` et au digest de la
  release (`updaterapi.CheckImage`, `selfupdate.go`).
- Toutes les actions épinglées par SHA, tous les `FROM` par digest ;
  Dependabot sur gomod, npm, docker et actions.
- Aucune clé privée dans l'arbre git ; clés écrites en 0600
  (`internal/pki`, `cmd/janusctl/context.go`).
- Publication du site et des .deb par comptes à commande forcée avec
  `restrict` et clé d'hôte épinglée (`site/deploy`, `packaging/apt`).
- Clé de signature Secure Boot via `mktemp` + `trap`, jamais dans un
  fichier du dépôt ; `docker logout` après le push.
- `govulncheck` : 0 vulnérabilité atteinte. GO-2026-5932
  (`x/crypto/openpgp`) est requis par lego et go-libvirt sans être
  appelé : rien à faire. `npm audit` : 0 sur `dashboard/frontend` et
  `site/frontend`.
- Le site public limite les builds par IP et globalement
  (`site/backend/api.go` `buildLimiter`) ; le jeton GitHub est lu d'un
  fichier 0640 hors du dépôt.

## Actions recommandées, par priorité

1. #1 : réactiver `CPU_MITIGATIONS` sur les deux pistes x86, puis une
   passe image-build complète (boot, hardening, perfs à mesurer).
2. #6 : contraindre la section `global` avant tout `haproxy -c`. Fait.
3. #2, #7, #8 : les trois corrections de workflows, sans effet fonctionnel.
   Fait (#8 : créer `APT_HOST_KEY`).
4. #3, #4, #5, #15 : les quatre corrections du Controller, vérifiées par
   `make qemu-dashboard-test`. Fait.
5. #12 : BGP/VRRP en `operators`, régénérer `api-reference.md`. Fait.
6. #9, #10, #11, #13, #14, #16 : au fil de l'eau. Restent #9 et #11.

## Comment rejouer les vérifications

```sh
go tool govulncheck ./...
(cd terraform-provider-janus && go run golang.org/x/vuln/cmd/govulncheck@latest ./...)
for d in dashboard/frontend site/frontend site/docs; do (cd $d && npm audit); done
grep -nE 'CONFIG_(CPU_MITIGATIONS|VMAP_STACK|IO_URING)\b' kernel/configs/janus_*_defconfig
grep -nE 'self:capability' selinux/policy.conf
```
