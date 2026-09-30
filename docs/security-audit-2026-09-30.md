# Audit sécurité / bugs / optimisations — code Go de Janus

Date : 2026-09-30
Périmètre : tout le code Go du dépôt (`cmd/janusd`, `cmd/janusctl`,
`internal/*`, `rootfs/init`, `dashboard/backend`). Le code généré
(`gen/`) et les scripts shell/image ne sont pas audités ligne à ligne,
seulement là où le code Go en dépend.
Méthode : lecture exhaustive des sources, `go build`/`go vet`/`go test`
(tous verts), `govulncheck ./...`.
Ce fichier est un rapport de travail réutilisable dans une autre
session : chaque entrée est autonome (emplacement, cause, impact,
correction proposée).

Statut de compilation au moment de l'audit : `go build ./...`,
`go vet ./...` et `go test ./...` passent. Aucune de ces découvertes
n'est un échec de test existant ; ce sont des angles que les tests
actuels ne couvrent pas.

## Suivi des corrections

Mis à jour le 2026-09-30, quatrième passe. Corrigés : #1 à #14, en
commits séparés par thème (`f3b4376` à `f581851`). #5 a été corrigé
selon l'option A, choisie par l'utilisateur. Chaque section concernée porte un paragraphe **Corrigé**
qui décrit la correction et sa vérification.

**Rotation de la clé de signature : terminée.** L'ancien certificat
portait `CN=HAProxyOS Secure Boot signing key`, généré avant le
renommage du projet. La nouvelle clé (`CN=Janus Secure Boot signing
key`, RSA 4096, empreinte SHA-256 `D0:D8:91:EF:…:1D:FF:0F`) a été générée
hors du dépôt, sauvegardée, mise dans le secret `SECUREBOOT_SIGNING_KEY`
le 2026-09-30, puis sa copie locale supprimée. Son certificat remplace
`image/secureboot/production-cert.pem` et est embarqué dans
`internal/releasetrust/certs/`. Le run `image-build` 36745534311 l'a
prouvé : son étape ISO a signé les deux UKI avec le secret et `sbverify`
les a validés contre le nouveau certificat.

**Régression corrigée après le push.** Le commit de dépendances
(`f3b4376`) avait laissé `go mod tidy` réécrire la directive `go 1.26` en
`go 1.26.0`. `setup-go`, qui lit `go.mod`, a alors installé exactement
Go 1.26.0 au lieu du dernier 1.26.x. Le lint a échoué (`buf` exige
≥ 1.26.7), et `vulncheck` a signalé des failles de la bibliothèque
standard corrigées en 1.26.6 (`net/url`, `html/template`, `crypto/tls`),
que les binaires d'`image-build.yml` auraient embarquées. Corrigé par
`toolchain go1.26.8` dans `go.mod` (`6e8edb1`), que `setup-go` utilise en
priorité ; la CI est repassée au vert. Le run `image-build` 36745534311,
lancé avant, a compilé `janusd` avec Go 1.26.0 : ses artefacts de
vérification ne doivent pas servir de livrable.

Nouveaux constats de cette passe, tous deux ouverts : #15 (rootfs
différent pour chaque artefact d'un même run CI) et #16 (bundle
incohérent qui peut rendre un nœud injoignable).

**Points ouverts notés pendant l'ajout de la configuration réseau**
(même jour, hors numérotation) :

- **DHCP noyau sans renouvellement.** `ip=dhcp` configure une seule
  interface au démarrage et ne renouvelle jamais le bail. Un nœud dont
  le serveur DHCP ne réserve pas l'adresse garde un bail expiré. Un
  client DHCP en espace utilisateur, avec renouvellement, est l'amélioration
  prévue (voir `docs/network-configuration.md`).
- **NTP non authentifié.** `janusd` interroge ses serveurs en SNTP. Un
  attaquant sur le chemin peut décaler l'horloge, donc les dates des
  certificats émis au premier démarrage. NTS serait la réponse, à un
  coût bien plus élevé.
- **Certificats clients sans renouvellement.** Le certificat serveur du
  nœud est désormais réémis 30 jours avant expiration, mais les
  certificats clients (admin, service du Controller) restent valables
  un an sans mécanisme de renouvellement : les premiers nœuds déployés
  les perdront vers septembre 2027.

Vérification globale après correction : `go build`, `go vet`,
`golangci-lint` (0 problème), `go test -race ./...` et `govulncheck`
(0 vulnérabilité atteinte, code de sortie 0) passent. Les corrections
du Controller ont aussi été essayées sur un `dashboardd` réel, et celles
du socket stats sur le vrai binaire HAProxy. Les scripts QEMU
(`hack/qemu-*`) n'ont pas été relancés : aucune valeur qu'ils envoient
n'est touchée par la nouvelle validation, et curl n'envoie ni `Origin`
ni `Sec-Fetch-Site`.

Plusieurs appréciations de la première passe se sont révélées fausses et
sont corrigées ci-dessous (#10 et #11 sont précisés dans leurs sections) :

- **#1 était surévalué.** Le panic grpc ne survient que sur un serveur
  configuré avec le routage xDS, que `janusd` n'utilise pas. Il
  n'était pas exploitable ici.
- **#3 était sous-évalué.** L'atténuation « préflight CORS » décrite
  n'existait pas. Les handlers ne vérifient pas le `Content-Type`, donc
  un `POST` `text/plain` sans préflight dont le corps est du JSON
  atteignait aussi reboot, reset, application de config et upgrade.
- **#3, lecture de fichiers : surévalué.** Une page tierce peut déclencher
  un `GET` mais ne peut pas lire la réponse (politique same-origin). Il
  n'y avait donc pas d'exfiltration possible par cette voie.

---

## Tableau de synthèse

| # | Sévérité | Statut | Type | Emplacement | Titre |
|---|----------|--------|------|-------------|-------|
| 1 | Info (réévaluée) | Corrigé | Vuln dépendance | `go.mod` (grpc 1.84.0) | Panic serveur gRPC (GO-2026-6443), non atteignable sans xDS |
| 2 | Élevée | Corrigé | Escalade de privilège | `internal/haproxy/*.go` | Injection de commandes socket stats HAProxy (dont reader→admin via `MapGet`) |
| 3 | Moyenne | Corrigé | CSRF | `dashboard/backend/internal/nodeproxy/*`, `dashboard/backend/main.go` | Endpoints destructifs sans protection anti-CSRF |
| 4 | Moyenne | Corrigé | Vuln dépendance | `go.mod` | `x/crypto` 0.54.0 et `klauspost/compress` 1.18.5 obsolètes |
| 5 | Moyenne | Corrigé (option A) | Robustesse | `internal/api/lifecycle.go` | `Upgrade` accepte `http://` sans signature (MITM du rootfs) |
| 6 | Faible | Corrigé | Durcissement | `dashboard/backend/internal/auth` | Pas de limitation de débit sur le login (brute-force) |
| 7 | Faible | Corrigé | Bug | `internal/api/system_files.go` | `Read` peut bloquer indéfiniment sur un FIFO |
| 8 | Faible | Corrigé | Robustesse | `dashboard/backend/internal/nodeproxy/system.go` | `handleMetrics` : seuil d'erreur codé en dur (`== 6`) |
| 9 | Faible | Corrigé | Fuite ressource potentielle | `dashboard/backend/internal/auth/auth.go` | Sessions en mémoire jamais balayées activement |
| 10 | Faible | Corrigé | Optimisation | `internal/api/system_proc.go` | `readProcesses` relit tout `/proc` à chaque poll de métriques |
| 11 | Faible (réévaluée) | Corrigé | Durcissement | `cmd/janusd/main.go` | Pas de limites gRPC (streams concurrents, taille message) |
| 12 | Info | Corrigé | Robustesse | `internal/api/lifecycle.go` | `writePartitionFile` n'appelle pas `f.Sync()` avant le `syscall.Sync()` global |
| 13 | Faible | Corrigé | Bug (trouvé en corrigeant #2) | `internal/haproxy/runtime_maps.go`, `runtime_certs.go` | Valeur de map tronquée au premier espace ; bundle PEM coupé par une ligne vide ou bloqué sans saut de ligne final |
| 14 | Moyenne | Corrigé | Chaîne de publication | `.github/workflows/image-build.yml` | Les GitHub Releases publient des UKI non signés alors qu'un bundle signé est construit |
| 15 | Faible | Ouvert | Chaîne de build | `Makefile` (`rootfs-build`), `rootfs/assemble.sh` | Chaque artefact d'un run CI embarque un rootfs différent (root hash différent) |
| 16 | Moyenne | Ouvert | Robustesse | `internal/api/lifecycle.go` | Un squashfs/verity qui ne correspond pas au UKI rend le nœud injoignable après reboot |

---

## 1. (Info, réévaluée) Panic serveur gRPC (GO-2026-6443)

- **Où** : dépendance `google.golang.org/grpc v1.84.0`, signalée par
  govulncheck via `cmd/janusd/main.go:188` (`srv.Serve(lis)`).
- **Cause** : la couche transport HTTP/2 acceptait des requêtes sans
  `:authority` ni `Host`. D'après l'avis officiel
  (`https://vuln.go.dev/ID/GO-2026-6443.json`), le panic se produit dans
  l'intercepteur de routage **xDS** (`internal/xds/server.RouteAndProcess`).
- **Impact réel** : aucun pour Janus. Le dépôt n'importe aucun paquet
  `grpc/xds` (vérifié par recherche). govulncheck signale la fonction de
  transport parce qu'elle est bien appelée, mais la condition du panic
  n'est pas réunie. La première passe l'avait classé « Élevée » à tort.
- **Corrigé** : aucune version stable de grpc ≥ 1.85 n'est publiée (seules
  des `-dev`). grpc est passé en **v1.83.2**, correctif stable de la
  branche 1.83 qui durcit aussi la couche transport. Build, vet, lint et
  tests passent, govulncheck ne signale plus rien. À faire plus tard :
  remonter en ≥ 1.85.0 dès sa sortie stable.
- **CI** : un job `vulncheck` a été ajouté à `.github/workflows/ci.yml`
  (`go run golang.org/x/vuln/cmd/govulncheck@latest ./...`). Il n'utilise
  ni secret ni runner self-hosted, donc il reste sûr pour les PR de
  forks. En mode par défaut, il n'échoue que sur une vulnérabilité
  réellement atteinte par le code.

## 2. (Élevée) Injection de commandes dans le socket stats HAProxy

- **Où** :
  - `internal/haproxy/manager.go` : `statsCommand(cmd)` fait
    `conn.Write([]byte(cmd + "\n"))` sans échappement.
  - `internal/haproxy/runtime_maps.go` : `MapGet` → `"show map " + mapName`,
    `MapUpdate` → `"add map %s %s %s"`, `ACLUpdate` → `"add acl %s %s"`.
  - `internal/haproxy/manager.go` : `SetServerState` →
    `"set server %s/%s state %s"`.
  - `internal/haproxy/runtime_certs.go` : `CertificateUpload`/`Delete`
    concatènent `name`, `crtList`, `sni`.
  - Le socket est déclaré `level admin` dans
    `rootfs/base/etc/haproxy/haproxy.cfg:9`.
- **Cause** : les arguments d'appel (nom de map, clé, valeur, nom de
  backend/serveur, nom de certificat, crt-list, SNI) proviennent
  directement des requêtes gRPC et sont interpolés dans la ligne envoyée
  au socket d'administration. La CLI HAProxy sépare plusieurs commandes
  par `;` sur une même ligne, et exécute une nouvelle ligne après un
  `\n`. Une valeur contenant `;` ou un saut de ligne permet donc
  d'exécuter une **commande admin arbitraire** du socket stats.
- **Impact** :
  - **Escalade reader→admin** : `MapGet` est classé `adminOrReader`
    (`internal/api/authz.go:87`). Un porteur de certificat `os:reader`
    peut appeler `MapGet` avec un nom de map tel que
    `x; set server be/s1 state maint` (ou `del map …`, `clear …`,
    `set …`) et faire exécuter une commande de niveau admin au socket,
    alors que son rôle ne l'autorise qu'à observer. C'est le point le plus
    grave : la frontière reader/admin, tout l'objet de `authz.go`, est
    contournable.
  - Pour un `os:admin`, l'injection reste dans le périmètre déjà autorisé,
    mais c'est une classe de bug à corriger sur le fond.
- **Reproduction (raisonnement)** : découle de la sémantique documentée
  de la CLI HAProxy (séparateur `;`, exécution ligne par ligne du socket)
  combinée à l'absence de validation. Non exécuté en mode attaque, mais
  l'analyse du code suffit à établir le vecteur.
- **Corrigé** : nouvelle barrière unique `internal/haproxy/cliarg.go`,
  appelée par chaque commande avant toute connexion au socket.
  - **Échappement plutôt que refus** : la CLI HAProxy interprète `\ `,
    `\;` et `\\`. C'est vérifié contre le binaire du projet
    (HAProxy 3.4.0) : `add map M k a\;b` stocke `a;b`. Chaque argument
    est donc échappé. Un appel produit toujours exactement une commande,
    et un `;` légitime dans une valeur est accepté.
  - **Refus de ce qui ne s'échappe pas** : tout caractère de contrôle
    (`\n`, `\r`, tabulation, NUL…), les noms ou clés vides, et les
    espaces dans les noms et clés. `show map` réaffiche les clés sans
    échappement, donc une clé avec espace ne se relirait pas sans
    ambiguïté.
  - **Payload PEM** (`set ssl cert … <<`) : les lignes vides sont
    supprimées au lieu de terminer le payload, les caractères de
    contrôle sont refusés, et la fin est toujours normalisée.
  - **Côté API** (`internal/api/haproxy.go`), un refus devient
    `codes.InvalidArgument` via `haproxyError`. `ServerSetState` refuse
    aussi explicitement un état non spécifié.
  - `MapGet` reste `adminOrReader` : un nom tel que `m;clear` part
    désormais en `show map m\;clear`, que HAProxy traite comme un nom de
    map inconnu. Vérifié sur le vrai binaire : erreur « Unknown map
    identifier », map intacte.
- **Tests** (`internal/haproxy/cliarg_test.go`, avec un faux socket stats
  qui enregistre les octets reçus) :
  - `TestStatsSocketUnescapableRefused` : 21 entrées piégées sur toutes
    les commandes sont refusées (`ErrInvalidArgument`) et le socket ne
    reçoit rien. Contrôle par mutation : en retirant la garde de
    `MapGet`, les cas `MapGet` échouent bien.
  - `TestStatsSocketEscaping` : octets exacts envoyés pour `;`, espaces et
    antislashs.
  - `TestStatsSocketValidArguments` : les commandes ordinaires sont
    inchangées.
- **Vérification réelle** (test jetable, supprimé après usage, contre
  `build/haproxy`) : map avec valeur `backend with spaces; and a
  semicolon` stockée et relue à l'identique, ACL ajout et suppression,
  upload d'un bundle `cert + ligne vide + clé` réellement servi en TLS
  pour son SNI, suppression du certificat, état de serveur modifié.

## 3. (Moyenne) CSRF sur les listeners par-nœud du Controller

- **Où** : `dashboard/backend/internal/nodeproxy/` — tous les handlers
  (`ops.go`, `system.go`, `lifecycle.go`, `pcap.go`). Authentification par
  certificat client TLS (`tls.RequireAndVerifyClientCert`,
  `nodeproxy.go:66`).
- **Cause** : l'accès à un listener par-nœud est authentifié uniquement
  par le certificat client que le navigateur présente. Un navigateur
  ré-attache automatiquement ce certificat à toute requête vers cette
  origine (`https://host:<port>`), y compris une requête déclenchée par un
  **autre** site. Aucun jeton anti-CSRF, aucun contrôle d'en-tête
  `Origin`/`Sec-Fetch-Site`, et le cookie `SameSite=Strict` du port
  principal ne protège pas ces ports (ils n'utilisent pas de cookie).
- **Impact (réévalué)** : une page web malveillante ouverte dans le même
  navigateur que l'opérateur pouvait déclencher tout endpoint mutant. Les
  handlers décodent le JSON sans vérifier le `Content-Type`, donc un
  `POST` `text/plain`, envoyé sans préflight CORS, avec un corps JSON
  suffisait. Étaient atteignables : `POST /api/system/power`
  (reboot/shutdown/reset avec effacement de STATE),
  `POST /api/system/services/{id}/{action}`, `POST /api/haproxy/config`,
  `POST /api/lifecycle/upgrade-url` et les mutations de
  maps/ACL/certificats. `GET /api/pcap` lançait aussi une capture, mode
  promiscuous compris. En revanche, la page tierce ne peut pas lire les
  réponses : pas d'exfiltration de fichiers par ce biais, contrairement à
  ce qu'affirmait la première passe.
- **Corrigé** :
  - **Listeners par-nœud** : le handler est construit par
    `nodeproxy.newHandler` et enveloppé dans `http.CrossOriginProtection`
    (stdlib Go ≥ 1.25). Toute requête navigateur non-sûre (hors
    GET/HEAD/OPTIONS) venant d'une autre origine est rejetée en 403,
    d'après `Sec-Fetch-Site` ou, à défaut, `Origin`. Cela inclut le
    « same-site », c'est-à-dire un autre port du même hôte. Les clients
    non navigateur (curl, scripts, tests QEMU) n'envoient pas ces
    en-têtes et ne sont pas affectés.
  - **`GET /api/pcap`** a un effet de bord, que la stdlib laisse passer par
    principe. Il a donc son propre contrôle (`sameOriginOrDirect` :
    `Sec-Fetch-Site` absent, `same-origin` ou `none`).
  - **Port principal du Controller** (`dashboard/backend/main.go`) : même
    enveloppe. Le cookie `SameSite=Strict` ne couvrait pas le
    « same-site ».
  - Vérifié que le frontend ne fait aucun appel mutant entre origines. La
    seule interaction entre origines est l'ouverture d'une page de nœud,
    une navigation `GET` qui reste autorisée.
- **Tests** : `dashboard/backend/internal/nodeproxy/csrf_test.go`
  (`TestPerNodeCSRF`) couvre cross-site, same-site, `Origin` étranger et
  pcap cross-site, tous refusés, ainsi que same-origin, sans en-têtes et
  `GET` en lecture, qui passent. Contrôle par mutation : sans
  l'enveloppe, les quatre cas non-sûrs passent à tort. Vérification
  réelle sur un `dashboardd` lancé en local : setup cross-site et
  `Origin` étranger refusés (403) sans effet, setup same-origin accepté
  (204), SPA servie en cross-site `GET` (200), `/register` sans en-têtes
  non affecté.

## 4. (Moyenne) Dépendances vulnérables : x/crypto et klauspost/compress

- **Où** : `go.mod`.
- **Constat govulncheck** (module requis, pas forcément appelé, mais à
  aligner) :
  - `golang.org/x/crypto v0.54.0` → corrigé en `v0.56.0`
    (GO-2026-6355, GO-2026-6354 ; et GO-2026-6303 en 0.55.0 ;
    GO-2026-5932 sans correctif à ce jour).
  - `github.com/klauspost/compress v1.18.5` → corrigé en `v1.18.7`
    (GO-2026-5841).
- **Impact** : `x/crypto` sert `bcrypt` (auth du Controller) et `go-pkcs12`.
  govulncheck ne relie pas ces failles à un appel du code Janus (« vous
  requérez mais n'appelez pas »), donc l'exposition directe est faible,
  mais l'hygiène de dépendances impose l'alignement.
- **Corrigé** : les dépendances ont été montées ainsi.

  | Module | Avant | Après |
  |---|---|---|
  | `golang.org/x/crypto` | v0.54.0 | v0.57.0 |
  | `github.com/klauspost/compress` | v1.18.5 | v1.20.1 |
  | `golang.org/x/net` (entraîné) | v0.57.0 | v0.58.0 |
  | `golang.org/x/sys` (entraîné) | v0.47.0 | v0.48.0 |
  | `golang.org/x/text` (entraîné) | v0.40.0 | v0.42.0 |

  Reste GO-2026-5932 (`x/crypto`) : aucun correctif publié, et le code
  Janus ne l'atteint pas. `go mod tidy` a normalisé la directive
  `go 1.26` en `go 1.26.0` (exigé par une dépendance, sans effet
  pratique). Le job CI `vulncheck` (voir #1) empêche désormais une
  régression.

## 13. (Faible) Bugs trouvés en corrigeant #2 — corrigés

- **Valeur de map tronquée** : la CLI HAProxy découpe les arguments aux
  espaces, donc `MapUpdate` avec `backend one` stockait seulement
  `backend`, sans erreur. Constaté sur le vrai binaire. Corrigé par
  l'échappement de #2, et `ACLUpdate` en bénéficie aussi.
- **Bundle PEM coupé** : une ligne vide entre certificat et clé, forme
  courante d'un `cat cert.pem key.pem`, terminait le payload
  `set ssl cert … <<`. La clé était alors perdue et ses lignes
  interprétées comme des commandes. Corrigé : les lignes vides sont
  supprimées.
- **Upload bloqué** : un bundle sans saut de ligne final laissait HAProxy
  attendre la fin du payload jusqu'au délai de 10 s du socket. Corrigé :
  la fin du payload est toujours normalisée.

## 5. (Moyenne) `Upgrade` accepte `http://` sans vérification de signature

- **Où** : `internal/api/lifecycle.go` — `fetchBundleFile` accepte un
  `bundleRef` en `http://` (branche `strings.HasPrefix(..., "http://")`),
  et le proto `ImageSource` documente « signature verification beyond the
  sha256 check isn't implemented yet ».
- **Statut** : **corrigé selon l'option A** (quatrième passe), voir le
  paragraphe **Corrigé** en fin de section. L'analyse ci-dessous est
  celle qui a motivé le choix.
- **Cause (précisée)** : le `sha256` fourni par l'appelant ne couvre que
  `rootfs.squashfs`. `rootfs.verity` et surtout `uki-<slot>.efi` ne sont
  vérifiés contre rien. Or le UKI contient le noyau, l'initrd et la ligne
  de commande, y compris le root hash dm-verity. Qui contrôle le UKI
  contrôle donc tout, même avec un squashfs intègre.
- **Impact** : en `http://`, un attaquant réseau remplace le UKI et obtient
  l'exécution de code persistante au reboot, sur tout nœud dont le
  firmware n'impose pas Secure Boot. Sur un nœud qui l'impose, le UKI
  falsifié est refusé au boot. Aucune entrée de secours n'existe alors
  sur l'ESP, et `rootfs/init`/`janusd` ne démarrent jamais pour revenir
  en arrière : le nœud reste **injoignable** (déni de service), même
  avec `wait_for_health`. Atténuations actuelles : RPC `adminOnly`, et le
  Controller pré-remplit l'URL `https://` de GitHub ; `http://` n'arrive
  que par saisie explicite de l'opérateur.
- **Propriété utile** : le root hash dm-verity est dans la ligne de
  commande **signée** du UKI. Authentifier le UKI authentifie donc tout
  le bundle, car le noyau vérifie ensuite chaque bloc du squashfs.
- **Options** :
  - **A. Vérifier la signature du UKI sur le nœud** avant d'écrire le slot
    (Authenticode contre `image/secureboot/production-cert.pem`, embarqué
    dans `janusd` à la compilation). Indépendant du transport : couvre
    `http://`, `https://`, le relais `UploadReleaseFile` et les chemins
    locaux. Réutilise la clé Secure Boot existante. Coûts : une
    dépendance ou un petit parseur PE ; une politique pour les builds de
    dev et les tests QEMU, dont les UKI ne sont pas signés (par exemple
    vérification active seulement si un certificat est embarqué) ;
    prérequis #14.
  - **B. Refuser `http://`** dans `Upgrade`. Simple, mais supprime la
    fonctionnalité de miroir LAN en `http://` et son test
    `qemu-lifecycle-upgrade-url-test`. Ne protège ni contre un miroir
    `https://` compromis, ni contre le relais ou les chemins locaux.
  - **C. Empreintes de tous les fichiers** passées par l'appelant
    (changement de proto, `SHA256SUMS` dans le bundle). Garde les miroirs
    `http://`, mais la confiance repose sur l'appelant qui doit obtenir
    les empreintes par un canal sûr.
- **Recommandation** : A, précédé de #14. C'est la seule option qui
  authentifie le contenu quelle que soit sa provenance.
- **Corrigé (option A)** :
  - Nouveau paquet `internal/releasetrust`. Les certificats de confiance
    sont embarqués dans `janusd` (`certs/*.pem`, un dossier pour
    permettre les rotations). La vérification Authenticode est complète
    et explicite : condensat de l'image égal au condensat signé,
    `messageDigest` égal au hash du `SpcIndirectDataContent`,
    `contentType` correct, signataire désigné par émetteur et numéro de
    série d'un certificat de confiance, signature RSA/SHA-256 valide. Les
    certificats inclus dans la signature sont ignorés. Un `recover`
    protège `janusd` d'un binaire malformé.
  - `Upgrade` télécharge et vérifie le UKI du slot cible **en premier**,
    avant le squashfs et avant toute écriture. `Install` vérifie les deux
    UKI avant de toucher au disque. Le résultat apparaît dans la
    progression et dans les événements (`signature`).
  - Opt-out explicite : champ proto `ImageSource.insecure_skip_signature_check`,
    drapeau `janusctl lifecycle upgrade|install
    -insecure-skip-signature-check`, et case décochée par défaut dans la
    vue Update du Controller, avec avertissement dans la confirmation.
  - Les scripts QEMU, qui construisent des bundles non signés, passent
    l'opt-out. `qemu-lifecycle-upgrade-https-test` le passe aussi, parce
    que les Releases publiées jusqu'ici ne sont pas signées ; à retirer
    dès qu'une Release signée existe.
- **Tests** :
  - `internal/releasetrust` : binaire valide, non signé, clé non
    approuvée, signé par un tiers, modifié après signature, entrées
    corrompues, interopérabilité avec le vrai `sbsign`, et égalité entre
    le certificat embarqué et celui qui signe en CI.
  - `internal/api` : `TestCheckUKISignature` (refus, opt-out,
    acceptation).
  - Les vrais UKI de 4,8 Mo, signés par la nouvelle clé de production,
    sont acceptés avec le certificat embarqué.
  - `qemu-lifecycle-upgrade-test`, sur un vrai nœud : sans l'opt-out, le
    bundle non signé est refusé en `FailedPrecondition`, rien n'est écrit
    et le nœud ne redémarre pas. Avec l'opt-out, l'upgrade vers le slot B
    puis le rollback passent. Réussi.
  - `lifecycle-install-test` (Install natif, puis boot du disque produit)
    et `qemu-lifecycle-upgrade-relay-test` (relais `UploadReleaseFile`)
    passent avec l'opt-out.
  - Non relancés : les autres tests QEMU. Leur seule modification est
    l'ajout du drapeau d'opt-out.

## 14. (Moyenne) Les GitHub Releases publiaient des UKI non signés — corrigé

- **Où** : `.github/workflows/image-build.yml`. L'étape « Build a real,
  production-signed release bundle » construit `build/release-signed/` et
  vérifie les deux UKI avec `sbverify`. L'étape de publication de la
  Release attache pourtant `build/release/uki-a.efi` et `uki-b.efi`, les
  versions **non signées**. Le bundle signé n'est publié nulle part.
- **Impact** : un nœud dont le firmware impose Secure Boot avec la clé de
  production refuserait de démarrer une Release publiée. Et aucune
  vérification de signature côté nœud (#5, option A) n'est possible
  contre les Releases actuelles.
- **La correction proposée en troisième passe était fausse.** Elle
  consistait à publier les UKI de `build/release-signed/` avec le
  squashfs de `build/release/`. Or les deux bundles ne partagent **pas**
  le même rootfs (voir #15) : le UKI signé aurait porté le root hash d'un
  autre squashfs, et le nœud n'aurait pas démarré.
- **Corrigé** : c'est le bundle réellement publié qui est signé, au moment
  où il est construit.
  - La cible `iso-image-with-bundle` accepte `SIGNING_KEY`/`SIGNING_CERT`
    (vides par défaut, donc non signé comme avant).
  - L'étape ISO du workflow signe avec le secret quand il existe, puis
    vérifie les deux UKI avec `sbverify`. L'ancienne étape qui signait
    un bundle jeté est supprimée.
  - Une étape « Require a signed release bundle » refuse de publier une
    Release dont les UKI ne se vérifient pas contre
    `production-cert.pem`.
  - Un UKI signé démarre aussi sur un firmware sans Secure Boot : la
    signature y est simplement ignorée.
- **Vérification locale** : la cible, avec une clé de test puis avec la
  nouvelle clé de production, produit des UKI que `sbverify` accepte, et
  `veritysetup verify` confirme que leur root hash correspond au
  squashfs publié. Sans clé, le bundle reste non signé. Le garde-fou
  refuse un bundle signé par une autre clé. Le workflow lui-même n'a
  pas pu être exécuté (dispatch self-hosted).

## 6. (Faible) Pas de limitation de débit sur le login du Controller

- **Où** : `dashboard/backend/auth_handlers.go` (`handleAuthLogin`),
  `dashboard/backend/internal/auth/auth.go` (`Verify`).
- **Cause** : `Verify` fait un `bcrypt.CompareHashAndPassword` sans aucun
  compteur d'échecs ni délai. Un compte unique, mot de passe ≥ 8
  caractères.
- **Impact** : brute-force en ligne possible sur le mot de passe admin.
  `bcrypt` (coût par défaut) ralentit, mais rien ne borne le nombre de
  tentatives.
- **Corrigé** : `dashboard/backend/internal/auth/limiter.go`
  (`LoginLimiter`), branché dans `handleAuthLogin`.
  - Limite **par adresse client**, pour qu'un tiers ne puisse pas bloquer
    l'opérateur depuis sa propre adresse. Derrière un reverse proxy, toutes
    les requêtes partagent l'adresse du proxy et la limite devient
    globale ; c'est noté dans le code.
  - 5 échecs gratuits, puis verrouillage doublé à chaque échec : 1 s,
    2 s, 4 s… jusqu'à 15 min. Réponse 429 avec `Retry-After`, vérifiée
    **avant** tout calcul bcrypt, donc une adresse verrouillée ne coûte
    rien en CPU.
  - Un succès efface le compteur. Les échecs vieux de plus d'une heure
    sont oubliés. La table est bornée à 10 000 adresses : au-delà, les
    entrées périmées puis les plus anciennes sont évincées.
  - Non traité : une attaque distribuée sur de nombreuses adresses, et le
    minimum de longueur du mot de passe (inchangé, 8).
- **Tests** : `auth_test.go` (`TestLoginLimiter` avec horloge injectée :
  seuil, doublement, plafond, isolation par adresse, remise à zéro,
  oubli ; `TestLoginLimiterBounded`). Vérification sur un `dashboardd`
  réel : 6 mauvais mots de passe renvoient 401, la tentative suivante
  429 avec `Retry-After: 1`, le bon mot de passe passe après l'attente.

## 7. (Faible) `Read` peut bloquer indéfiniment sur un FIFO

- **Où** : `internal/api/system_files.go` — `Read`.
- **Cause** : `Read` refuse les périphériques bloc/caractère
  (`fs.ModeDevice|fs.ModeCharDevice`) mais pas les FIFO
  (`fs.ModeNamedPipe`). `os.Open` sur un FIFO sans écrivain bloque
  jusqu'à ce qu'un écrivain apparaisse ; l'appel gRPC reste pendu (borné
  seulement par le timeout côté client).
- **Impact** : DoS ponctuelle d'un handler (goroutine bloquée), `adminOnly`.
  Faible.
- **Corrigé** : `Read` ouvre en `O_NONBLOCK`, puis vérifie le type sur le
  descripteur ouvert (`f.Stat()`) : seuls les fichiers réguliers sont lus.
  Cela ferme aussi la fenêtre entre la vérification et l'ouverture qui
  existait avec `os.Stat` puis `os.Open`. Les fichiers `/proc` et `/sys`,
  réguliers de taille 0, restent lisibles.
- **Tests** : `TestRead` ajoute un FIFO sans écrivain (refus immédiat, et
  échec en 5 s sur l'ancien code, vérifié par mutation), un lien
  symbolique vers un fichier, et `/proc/self/stat`.

## 8. (Faible) `handleMetrics` : seuil d'erreur codé en dur

- **Où** : `dashboard/backend/internal/nodeproxy/system.go` —
  `if len(out.Errors) == 6`.
- **Cause** : le nombre `6` doit rester synchronisé manuellement avec le
  nombre d'appels `fetch(...)` (system/memory/load/network/services/
  haproxy). Ajouter/retirer une métrique casse silencieusement la
  détection « nœud injoignable » (renverra 200 avec des données
  partielles au lieu de 502).
- **Impact** : robustesse/maintenance, pas de faille. Faible.
- **Corrigé** : `fetch` compte les appels lancés, et le test
  « injoignable » compare à ce compte.

## 9. (Faible) Sessions en mémoire jamais balayées activement

- **Où** : `dashboard/backend/internal/auth/auth.go` — `sessions
  map[string]time.Time`.
- **Cause** : les entrées expirées ne sont supprimées qu'à la lecture
  (`ValidSession`). Un jeton jamais réutilisé (ex. login puis fermeture du
  navigateur) reste en mémoire jusqu'au redémarrage.
- **Impact** : croissance mémoire théorique, bornée par le nombre de
  logins réussis (compte unique, faible volume). Négligeable en pratique.
- **Corrigé** : `NewSession` purge les sessions expirées. La table est
  ainsi bornée aux sessions créées pendant une durée de vie de session.
  Test : `TestSessionsSweptOnCreate`.

## 10. (Faible) `readProcesses` relit tout `/proc` à chaque poll

- **Où** : `internal/api/system_proc.go` — `readProcesses`, appelé par
  `Processes` et `Stats` ; `Stats` est appelé par le poll de métriques du
  dashboard (`handleMetrics`, jusqu'à toutes les secondes).
- **Cause** : parcours complet de `/proc` + lecture de `stat`/`cmdline`
  par PID à chaque appel. Pour `Stats`, seuls `janusd`/`haproxy` sont
  retenus, mais tout `/proc` est quand même lu.
- **Impact** : coût CPU/IO récurrent modeste sur un nœud à faible charge
  processus ; non critique, mais c'est le chemin le plus chaud exposé par
  le dashboard.
- **Corrigé, autrement que proposé** : se limiter aux PID connus aurait
  faussé `Stats`. Après un rechargement, l'ancien haproxy qui termine ses
  connexions doit être compté, et le `Manager` ne suit que le processus
  courant. Le balayage de `/proc` est donc conservé, mais `Stats` ne lit
  plus que `/proc/<pid>/stat`, qui contient déjà le nom du processus, et
  plus `cmdline`. `Processes` garde la ligne de commande complète.
- **Mesure** (benchmark jetable, environ 100 processus) :

  | Chemin | Par appel |
  |---|---|
  | Avant (`stat` + `cmdline`) | 2,2 ms |
  | Après (`stat` seul) | 1,2 ms |

## 11. (Info) Aucune limite gRPC configurée sur le serveur

- **Où** : `cmd/janusd/main.go` — `grpc.NewServer(...)` ne fixe ni
  `MaxConcurrentStreams`, ni `MaxRecvMsgSize`, ni politique de keepalive
  (`KeepaliveEnforcementPolicy`).
- **Cause** : valeurs par défaut de gRPC (message reçu 4 MiB par défaut,
  streams concurrents non bornés côté serveur selon version).
- **Impact** : surface d'abus (streams multiples, keepalive agressif) pour
  un client authentifié. `UploadReleaseFile` chunk à 512 KiB donc reste
  sous 4 MiB : OK. Info/durcissement.
- **Impact réel identifié en corrigeant** : le risque concret n'était pas
  l'abus, mais les flux suivis (`Events`, `Logs`, `Dmesg` avec follow,
  `PacketCapture` sans durée) d'un client disparu sans fermer sa
  connexion TCP : hôte éteint, coupure réseau, entrée NAT expirée. Le
  contexte serveur n'était jamais annulé, et le flux, ou la capture,
  tournait jusqu'aux timeouts TCP du noyau, soit des heures.
- **Corrigé** : `cmd/janusd/main.go`, `connectionOptions`.
  - Keepalive serveur : ping après 2 min sans trafic reçu, connexion
    fermée si pas de réponse en 20 s, ce qui annule ses flux. Les clients
    gRPC répondent seuls aux pings ; rien à changer côté `janusctl` ou
    Controller.
  - `MaxConcurrentStreams` à 256 par connexion : un garde-fou très
    au-dessus de l'usage du Controller, qui multiplexe toutes les pages
    ouvertes sur une connexion.
  - `MaxRecvMsgSize` laissé à la valeur par défaut (4 MiB), suffisante
    pour les chunks de 512 KiB d'`UploadReleaseFile`.
- **Tests** : `cmd/janusd/keepalive_test.go` fait passer un flux `Events`
  suivi par un proxy TCP qui gèle la connexion sans la fermer. Avec les
  options (délais réduits à 1 s), le flux se termine côté serveur. Un cas
  témoin sans les options confirme qu'il resterait ouvert. Le test est
  stable sur 3 exécutions sous `-race` et dure environ 6 s ; il est
  ignoré avec `-short`.

## 12. (Info) `writePartitionFile` : pas de `f.Sync()` avant le sync global

- **Où** : `internal/api/lifecycle.go` — `writePartitionFile` écrit puis
  `defer f.Close()` ; le `syscall.Sync()` global suit dans `Upgrade`.
- **Cause** : l'écriture du device de partition n'est pas explicitement
  `f.Sync()`-ée ; on s'appuie sur le `syscall.Sync()` global ensuite. En
  pratique correct (le `syscall.Sync()` global couvre tout avant reboot),
  mais dépend de l'ordre. `UploadReleaseFile` fait bien `f.Sync()` pour
  son cas.
- **Impact** : très faible, robustesse. Le `syscall.Sync()` global couvre
  le cas nominal.
- **Corrigé** : `writePartitionFile` appelle `f.Sync()` puis `f.Close()`,
  et remonte leurs erreurs au lieu de laisser un `defer f.Close()` les
  ignorer. Une erreur d'écriture que la couche bloc ne signale qu'au
  `fsync` fait désormais échouer l'`Upgrade` **avant** que l'ESP ne bascule
  sur ce slot. Pas de test unitaire : il faudrait un vrai périphérique
  bloc. C'est couvert par les tests QEMU d'upgrade, non relancés ici.

## 15. (Faible) Un rootfs différent pour chaque artefact d'un même run CI — ouvert

- **Où** : `Makefile` (`rootfs-build` est `.PHONY` et chaque cible
  d'image en dépend) et `rootfs/assemble.sh` (`mksquashfs` sans
  horodatage fixe).
- **Constat** : dans un même run d'`image-build.yml`, le qcow2, le vmdk,
  l'ISO et le bundle de la Release sont construits chacun sur un rootfs
  reconstruit, donc avec un root hash différent. Le contenu source est le
  même, mais les octets diffèrent.
- **Impact** : traçabilité (deux artefacts « v2026.09.30 » ne sont pas
  identiques) et piège pour tout mélange de fichiers entre artefacts :
  c'est ce qui a invalidé la première correction proposée pour #14.
- **Correction proposée** : un rootfs reproductible (horodatages fixes via
  `SOURCE_DATE_EPOCH`, `-mkfs-time`/`-all-time` de `mksquashfs`,
  sel verity fixé), ou une cible `rootfs-build` non `.PHONY` construite
  une seule fois par run.

## 16. (Moyenne) Un bundle incohérent peut rendre un nœud injoignable — ouvert

- **Où** : `internal/api/lifecycle.go`, `Upgrade`.
- **Constat** : la signature authentifie le UKI, donc le root hash
  attendu, mais `Upgrade` ne vérifie pas que `rootfs.squashfs` et
  `rootfs.verity` y correspondent avant de basculer. Un squashfs altéré
  en transit, ou un mélange de fichiers (voir #15), ne peut plus donner
  l'exécution de code : dm-verity le refuse. Mais le noyau ne monte
  alors pas la racine, `rootfs/init` et `janusd` ne démarrent jamais, et
  aucun mécanisme `wait_for_health` ne peut revenir en arrière.
- **Correction proposée** : avant la bascule de l'ESP, recalculer l'arbre
  dm-verity (SHA-256, blocs de 4 Kio, sel lu dans le superbloc de
  `rootfs.verity`) et comparer sa racine au root hash lu dans la ligne
  de commande **signée** du UKI. Le bundle devient alors vérifié de bout
  en bout avant tout reboot.

---

## Points vérifiés et jugés corrects (pour éviter de les re-signaler)

- **mTLS** obligatoire partout (`internal/pki/tls.go`, TLS 1.3 min, pas de
  fallback plaintext). `janusd` et les listeners par-nœud exigent bien un
  certificat client vérifié.
- **`authz.go`** couvre chaque RPC (fail-closed vers `adminOnly` par
  défaut, test `TestRequiredRolesCoversEveryRPC`). Seule réserve : le
  couple `MapGet=adminOrReader` + injection (voir #2).
- **Rôles lus depuis `Subject.Organization`** du certificat vérifié :
  correct, non falsifiable sans la clé CA du nœud.
- **`internal/pcapfilter`** : compilation BPF en sous-ensemble strict,
  refus de tout ce qui n'est pas supporté (pas d'approximation), jumps
  bornés à 255 avec erreur explicite. Solide.
- **`internal/ring`** : suivi par numéro de séquence, verrouillage
  correct, cap de ligne (`maxLine`) contre un writer qui n'émet jamais de
  `\n`. Correct.
- **PKI** : ECDSA P-256, SAN incluant les IP locales, séparation
  clé/cert, impression unique des identifiants au premier boot puis
  capture des logs seulement après (la clé admin ne transite pas dans le
  buffer lisible via `Logs`). Correct.
- **`Reset`/`wipeDirContents`** : préserve `lost+found`, vide les
  sous-répertoires bind-montés. Correct.
- **`store`/`pending`** : fichiers 0600 pour le matériel de clé, meta
  séparée. Correct.
- **`Supervisor`** : reap unique via `Wait4(-1)`, exigence `*os.File` sur
  Stdout/Stderr documentée (évite la fuite de goroutine). Correct.
- **`Copy`/`List`/`DiskUsage`** : ne descendent pas dans `/proc`,`/sys`,
  `/dev` ; `Copy` copie exactement la taille annoncée
  (`io.CopyN(tw, f, hdr.Size)`). Correct.

## Actions recommandées, par priorité

Faits : #1 à #14, et le job CI `vulncheck`.

Restant :

1. Publier une première Release signée, puis retirer l'opt-out de
   `qemu-lifecycle-upgrade-https-test`.
2. #16 : vérifier l'arbre dm-verity contre le root hash signé avant de
   basculer.
3. #15 : rendre le rootfs reproductible, ou le construire une seule fois
   par run.
4. Remonter grpc en ≥ 1.85.0 dès sa sortie stable (voir #1).
5. Relancer toute la suite QEMU avant une release : les chemins
   `Upgrade`/`Install` et le Controller ont changé.

## Comment rejouer les vérifications

```sh
go build ./... && go vet ./... && go test -race ./...
golangci-lint run ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```
