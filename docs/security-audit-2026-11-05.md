# Revue de sécurité défensive — ce qui est arrivé depuis le 2026-10-07

Date : 2026-11-05
Périmètre : tout ce qui a été ajouté ou changé depuis la revue du
2026-10-07 (commit `b79aee83` → `d909f888`, environ 72 000 lignes de Go
hors tests et code généré, plus la CI, les Dockerfiles, la politique
SELinux et les configs noyau). Les nouveautés sont surtout :
- les clusters (certificats de membre, verrous, pulls d'état, handoff) ;
- l'appairage par code et le volume de secours ;
- le standby du Controller et son témoin ;
- le SSO/OIDC ;
- le relais ACME et les certificats commandés par le Controller ;
- l'export et l'import d'état ;
- le test de connectivité ;
- le client DHCP de janusd ;
- les releases de maintenance ;
- le miroir et les compteurs du site ;
- le plugin Claude Code de janusctl.

Ce document ne reprend pas les constats des revues du 2026-09-30 et du
2026-10-07. Il signale seulement là où leur correctif ne tient plus (#4).

Méthode :
- six relectures en parallèle, une par zone : confiance du nœud,
  clusters, code réseau du nœud, authentification du Controller, infra
  du Controller, chaîne d'approvisionnement/CI/CLI ;
- puis une relecture du code de chaque constat retenu, qui en confirme
  le chemin ;
- `govulncheck` sur les deux modules Go et `npm audit` sur les quatre
  frontends.

Pas de code d'exploitation. Aucun constat n'a été reproduit sur un
nœud : chaque entrée donne l'emplacement, le risque, la gravité, le
correctif, et dit ce que le test du correctif doit prouver.

Outillage :
- `go tool govulncheck ./...` (module racine) : 0 vulnérabilité
  atteinte. Un avis au niveau module, GO-2026-5932
  (`golang.org/x/crypto/openpgp`, paquet abandonné), concerne un paquet
  que le code n'importe pas.
- Module `terraform-provider-janus` : 0.
- `npm audit --package-lock-only` : 0 sur `dashboard/frontend`,
  `site/frontend`, `site/docs` et `hack/browser`.

## Suivi des corrections

Les douze constats élevés et moyens hors décision ont été corrigés et
publiés dans la release **v2026.11.05** (2026-11-05), chacun avec son
enregistrement `security/fixes/JANUS-*` :

| # | Enregistrement | PR |
|---|---|---|
| 1 | JANUS-2026-008 | #537 |
| 2 | JANUS-2026-009 | #538 |
| 3 | JANUS-2026-018 | #547 |
| 4 | JANUS-2026-010 | #539 |
| 6 | JANUS-2026-017 | #548 |
| 7 | JANUS-2026-011 | #540 |
| 8 | JANUS-2026-013 | #542 |
| 9 | JANUS-2026-016 | #546 |
| 10 | JANUS-2026-015 | #544 |
| 11 | JANUS-2026-014 | #543 |
| 13 | JANUS-2026-012 | #541 |
| 14 | *(site, pas d'enregistrement node/controller/client)* | #545 |

Prouvés sur de vrais nœuds enforcing et en campagne lab
(`docs/pre-release-tests/v2026.11.05.md`).

Les constats faibles (#15 à #27) et les premiers points de
durcissement (#28) ont été corrigés et publiés dans la release **v2026.11.05-2**
(2026-11-06) - prouvés de même (`docs/pre-release-tests/v2026.11.05-2.md`) :

| # | Enregistrement | PR |
|---|---|---|
| 15 | JANUS-2026-019 | #555 |
| 16 | JANUS-2026-028 | #564 |
| 17 | JANUS-2026-029 (classé moyen) | #565 |
| 18 | JANUS-2026-030 | #565 |
| 19 | JANUS-2026-026 (classé moyen) | #563 |
| 20, 21 | JANUS-2026-023 | #559 |
| 22 | JANUS-2026-020 | #556 |
| 23 | JANUS-2026-024 (classé moyen) | #560 |
| 24 | JANUS-2026-021 (classé moyen) | #557 |
| 25 | JANUS-2026-025 (classé moyen) | #561 |
| 26 | JANUS-2026-022 | #558 |
| 27 | JANUS-2026-027 | #562 |
| 28 | *(durcissement, pas d'enregistrement ; la suite en v2026.11.08)* | #566 |

Les deux constats qui demandaient une décision ont été tranchés le
2026-11-06 et corrigés dans la release **v2026.11.07** (2026-11-07,
`docs/pre-release-tests/v2026.11.07.md`) :

| # | Décision | Enregistrement | PR |
|---|---|---|---|
| 5 | `os:admin` = toutes les clés du nœud, sauf son identité : la clé de sa CA ne part qu'avec un certificat de la CA du nœud ou de la racine de la flotte (le kit, `janusctl ... -kit`) | JANUS-2026-031 | #572 |
| 12 | Les codes masqués pour tous, admins compris : ils se lisent sur la console de l'hyperviseur | JANUS-2026-032 | #571 |

La release **v2026.11.08** (2026-11-08,
`docs/pre-release-tests/v2026.11.08.md`) complète #5 et traite le #28 :

| # | Enregistrement | PR |
|---|---|---|
| 5 (complément) | JANUS-2026-033 | #582 |
| 28 | *(durcissement, pas d'enregistrement)* | #583, #584, #585, #586 |

Le dernier point du #28 - les noms des services Consul - est corrigé sur
`main` (#588) et part dans la release qui suit v2026.11.08.

**Tous les constats de cette revue sont corrigés**, les points de
durcissement du #28 traités ou relus et gardés.

## Tableau de synthèse

| # | Gravité | Statut | Thème | Emplacement | Titre |
|---|---------|--------|-------|-------------|-------|
| 1 | Élevée | Corrigé (JANUS-2026-008) | Escalade de privilège | `selinux/policy.conf`, `internal/haproxy/globalcheck.go` | Une configuration HAProxy peut lire tout STATE, clé de la CA du nœud comprise |
| 2 | Élevée | Corrigé (JANUS-2026-009) | Autorisation | `dashboard/backend/acme_order.go` | Le Controller commande un certificat pour tout nom qu'un nœud demande |
| 3 | Moyenne | Corrigé (JANUS-2026-018) | Chaîne de publication | `.github/workflows/schematic-build.yml` | `source_run` signe avec la clé de production les entrées de n'importe quel run |
| 4 | Moyenne | Corrigé (JANUS-2026-010) | Durcissement nœud | `internal/haproxy/globalcheck.go` | La contrainte de la section `global` se contourne par les guillemets et les blocs `.if` |
| 5 | Moyenne | Corrigé (JANUS-2026-031, -033) | Secrets | `internal/api/system_state.go`, `state_grant.go`, `cluster.go` | Un `os:admin` direct emporte les clés des sites servis |
| 6 | Moyenne | Corrigé (JANUS-2026-017) | Clusters | `internal/cluster/cluster.go`, `internal/clusters/engine.go` | Un membre retiré garde son accès de membre |
| 7 | Moyenne | Corrigé (JANUS-2026-011) | Réseau | `internal/netmgr/dhcp.go`, `netmgr.go` | Nom d'hôte et domaine DHCP appliqués sans validation |
| 8 | Moyenne | Corrigé (JANUS-2026-013) | RBAC | `dashboard/backend/cluster_api.go` | Les lecteurs du Controller lisent les versions de cluster non masquées |
| 9 | Moyenne | Corrigé (JANUS-2026-016) | RBAC | `internal/rbac/rbac.go`, `internal/api/haproxy_runtime.go` | Les clés des stick tables sont servies telles quelles aux lecteurs |
| 10 | Moyenne | Corrigé (JANUS-2026-015) | Journalisation | `dashboard/backend/auth_handlers.go` | Journal d'audit vidable sans authentification, lignes forgées dans le log |
| 11 | Moyenne | Corrigé (JANUS-2026-014) | SSRF | `dashboard/backend/internal/nodeproxy/relay.go` | Le relais de mise à jour télécharge toute URL avant le contrôle de rôle |
| 12 | Moyenne | Corrigé (JANUS-2026-032) | Secrets | `dashboard/backend/console.go` | Les codes de secours et d'appairage passent par la console relayée |
| 13 | Moyenne | Corrigé (JANUS-2026-012) | Outillage | `cmd/janusctl/effect_hook.go` | Le hook du plugin autorise tout exécutable nommé `janusctl` |
| 14 | Moyenne (plausible) | Corrigé (PR #545) | Site | `site/backend/api.go` | Limite de débit des builds fondée sur un `X-Forwarded-For` usurpable |
| 15 | Faible | Corrigé (JANUS-2026-019) | Robustesse | `internal/probe/icmp.go` | Un paquet ICMP forgé fait tomber janusd pendant un ping |
| 16 | Faible | Corrigé (JANUS-2026-028) | Durcissement nœud | `internal/statexfer`, `internal/haproxy/manager.go` | Un import d'état pose un `haproxy.cfg` jamais vérifié au démarrage |
| 17 | Faible | Corrigé (JANUS-2026-029, classé moyen) | Disponibilité | `internal/pairing/window.go` | N'importe qui sur `:9506` ferme la fenêtre d'appairage ou de secours |
| 18 | Faible | Corrigé (JANUS-2026-030) | Durcissement nœud | `cmd/janusd/main.go` | `firewall: skip` dure tout le démarrage, pas la seule fenêtre de secours |
| 19 | Faible | Corrigé (JANUS-2026-026, classé moyen) | Secrets | `internal/logship/config.go`, `internal/clusters/balancer.go`, autres | Un secret « en écriture seule » suit un changement d'URL ou de confiance TLS |
| 20 | Faible | Corrigé (JANUS-2026-023) | RBAC / ressources | `internal/api/system_logs.go` | `Logs` d'un démarrage précédent : journal noyau aux opérateurs, `tail` sans plafond |
| 21 | Faible | Corrigé (JANUS-2026-023) | Secrets | `internal/api/probe.go`, `internal/haproxy/redact.go`, `haproxy.go` | Fuites vers les lecteurs : cible de la sonde, trous du masquage, empreinte non masquée |
| 22 | Faible | Corrigé (JANUS-2026-020) | Authentification | `dashboard/backend/internal/auth`, `cli_browser.go`, `sso_settings.go` | Sessions et certificats qui survivent à un compte supprimé, rétrogradé ou désactivé |
| 23 | Faible | Corrigé (JANUS-2026-024, classé moyen) | Disponibilité | `dashboard/backend/internal/sso/logins.go` | La table des connexions SSO en attente se remplit depuis peu d'adresses |
| 24 | Faible | Corrigé (JANUS-2026-021, classé moyen) | Secrets | `dashboard/backend/internal/hypervisor/store.go` | Clé SSH libvirt et jeton Proxmox en clair dans le répertoire de données |
| 25 | Faible | Corrigé (JANUS-2026-025, classé moyen) | Outillage | `cmd/janusctl/effect_hook.go`, `effects.go` | Hook du plugin : commande dans une variable non vue, effets mal classés |
| 26 | Faible | Corrigé (JANUS-2026-022) | CI | `.github/workflows/image-build.yml`, `release-promote.yml` | Release et pré-release publiables depuis n'importe quelle branche |
| 27 | Faible | Corrigé (JANUS-2026-027) | Robustesse | `internal/api/cluster_pull.go`, `site/backend/github.go` | Plafonds manquants : pull de cluster, recherches GitHub du site |
| 28 | Info | Traité (PR #566, #583-#586, #588) | Divers | voir la section | Points de durcissement sans risque direct |

## 1. (Élevée) Une configuration HAProxy peut lire tout STATE, clé de la CA du nœud comprise

- **Où** :
  - `selinux/policy.conf:553-555` donne à `haproxy_t` la lecture de
    tout fichier `state_t`. La justification ne vaut que pour
    `crt /etc/haproxy/acme`.
  - `:509` lui donne `dac_override` et `dac_read_search`.
  - STATE est monté avec un seul `context=state_t` (pas de xattr sur ses
    inodes), donc tout y porte ce type :
    - `pki/ca.key` et `server.key` ;
    - `haproxy/cluster/member.pem` et l'anneau des clés de tickets TLS ;
    - `config/acme/`, `config/logforward/`, le jeton du Controller.
  - `internal/haproxy/globalcheck.go` ne contrôle aucun chemin.
  - `internal/rbac/rbac.go:106` ouvre `ValidateConfig` aux lecteurs.
- **Risque** :
  - HAProxy lit sa configuration en root, avant `chroot` et la baisse de
    privilèges. Plusieurs directives chargent un fichier à l'analyse et
    en rendent le contenu visible : réponses servies, fichiers de motifs
    de maps/ACL que l'API runtime relit.
  - Un `os:operator` qui applique une configuration peut donc obtenir
    les octets de n'importe quel fichier de STATE. Cela comprend un
    opérateur du Controller sur la page d'un nœud et un jeton opérateur.
  - Avec la clé de la CA du nœud, il signe un certificat `os:admin` de
    ce nœud. Avec `member.pem`, il appelle `ClusterExport` sur chaque
    membre et obtient toutes les clés TLS du cluster.
  - Cela contredit deux règles : « les RPC de fichiers ne servent jamais
    les secrets », et un opérateur n'est pas admin.
  - En lecteur, `ValidateConfig` exécute `haproxy -c` dans le même
    domaine et renvoie les erreurs telles quelles. Ce qu'elles citent
    d'un fichier n'a pas été mesuré.
  - Trouvé par deux relectures indépendantes. La règle SELinux date du
    2026-09-24, donc aucune release n'en est exempte.
  - CVSS:3.1/AV:N/AC:L/PR:H/UI:N/S:C/C:H/I:H/A:N (8.7).
- **Correctif** (les deux, le second en défense en profondeur) :
  - Lancer HAProxy, `haproxy -c` compris, dans un espace de montage où
    `/etc/janus/pki`, `/etc/.state` et les répertoires de janusd
    n'existent pas. Seuls restent `/etc/haproxy` (configuration,
    `files/`, `acme/`, certificats runtime, `cluster/` dont HAProxy a
    besoin) et `/etc/ssl/certs`. janusd peut le préparer en se
    ré-exécutant en lanceur avant le `exec` d'haproxy : il a déjà
    `sys_admin`.
  - Dans la Policy, n'accepter un argument de directive qui charge un
    fichier que sous ces mêmes répertoires.
  - Passer `ValidateConfig` aux opérateurs.
  - Le test du correctif : une configuration d'opérateur qui nomme un
    fichier hors de ces répertoires est refusée. En enforcing, HAProxy
    ne voit plus `pki/`.

## 2. (Élevée) Le Controller commande un certificat pour tout nom qu'un nœud demande

- **Où** : `dashboard/backend/acme_order.go:172-270`,
  `internal/acmeorder/acmeorder.go:59-66`,
  `internal/acme/manager.go:67` (`HTTP01Rule`).
- **Risque** :
  - Le mode « commandé par ce Controller » se choisit par
    `ACMEApplyConfig`, une RPC d'opérateur. Le seul lien vérifié est
    l'empreinte du compte ACME du Controller, qui est publique (servie
    aux pages des nœuds).
  - Tout le reste vient de la configuration du nœud :
    - les noms du certificat ;
    - le défi ;
    - le fournisseur DNS, désigné par son seul nom ;
    - l'annuaire ACME et sa CA.
  - En DNS-01, le Controller répond avec ses propres identifiants DNS
    pour tout nom de la zone qu'ils couvrent, joker compris. La
    validation du nœud accepte `*.zone` (`internal/acme/config.go:240-245`).
  - En HTTP-01, chaque nœud en mode commandé répond à n'importe quel
    jeton avec l'empreinte du Controller. L'opérateur du nœud A peut
    donc obtenir un certificat pour un nom que sert le nœud B.
  - Résultat : des certificats de confiance publique pour les noms
    d'autres nœuds, d'autres équipes ou du Controller lui-même. La clé
    reste dans un HAProxy que l'opérateur contrôle. Cela contourne le
    découpage par labels et par domaines.
  - Effets de bord :
    - un SSRF aveugle (le Controller va chercher l'annuaire que le nœud
      nomme, et l'erreur revient au nœud) ;
    - un blocage de la passe : les commandes sont sérielles, 15 minutes
      chacune.
  - CVSS:3.1/AV:N/AC:H/PR:L/UI:N/S:C/C:L/I:H/A:N (7.1).
- **Correctif** :
  - Une politique fixée par un admin du Controller :
    - par fournisseur DNS, les zones ou suffixes qu'il peut servir ;
    - par sélecteur de labels, les noms qu'un nœud peut demander ;
    - une liste d'annuaires ACME permis (par défaut Let's Encrypt
      production et staging).
  - Refuser une commande hors de cette politique avant de lancer
    `janus-acme`.
  - Pour HTTP-01, en plus, n'accepter que les noms permis au nœud.
  - Commandes par nœud, avec une concurrence bornée.

## 3. (Moyenne) `source_run` signe avec la clé de production les entrées de n'importe quel run

- **Où** : `.github/workflows/schematic-build.yml:64`, `:134-157`,
  `:211-219`, et `image/schematic/build.sh`.
- **Risque** :
  - `source_run` doit seulement être numérique. L'artefact
    `schematic-inputs` est pris dans n'importe quel run du dépôt, y
    compris le run `pull_request` d'un fork qui a modifié `ci.yml` dans
    sa PR.
  - Le noyau et les tars de ces entrées sont ensuite assemblés et signés
    avec `SECUREBOOT_SIGNING_KEY`. Le résultat reste 7 jours en artefact
    téléchargeable.
  - Un bundle ainsi signé passe `releasetrust` (Upgrade/Install) et
    Secure Boot sur tous les nœuds.
  - Il faut qu'un mainteneur lance le workflow avec ce numéro, ou le
    jeton GitHub du site : il peut lancer des workflows
    (`site/deploy/setup-ct.sh:14`). Une compromission du site suffit
    donc.
  - Le code est antérieur au 2026-10-07, mais les deux revues
    précédentes ne le citent pas.
  - CVSS:3.1/AV:N/AC:H/PR:N/UI:R/S:C/C:N/I:H/A:N (6.1).
- **Correctif** :
  - Ne jamais donner la clé de production à un build `source_run` :
    clé jetable de `gen-test-key.sh` (ces builds ne sont jamais publiés).
  - Vérifier par l'API que le run vient de `image-build.yml`, en
    `workflow_dispatch`, depuis ce dépôt.
  - Refuser `source_run` quand l'acteur est le compte du site.

## 4. (Moyenne) La contrainte de la section `global` se contourne par les guillemets et les blocs `.if`

- **Où** : `internal/haproxy/globalcheck.go:74-127`. Ce correctif date
  de la revue du 2026-10-07 (#6).
- **Risque** :
  - La Policy découpe chaque ligne par `strings.Fields`. HAProxy, lui,
    retire les guillemets et les échappements de chaque mot, le premier
    compris, et honore les blocs `.if/.elif/.else/.endif` (depuis 2.4).
  - Un `chroot`, un `uid` ou un `gid` exigé peut donc être placé dans un
    bloc faux : la Policy le compte, HAProxy l'ignore.
  - Un mot-clé interdit entre guillemets n'est pas vu : un second
    `stats socket` sur une adresse réseau, `daemon`, `master-worker`,
    `insecure-fork-wanted`. Les contrôles des pairs (`peers janus`, port
    des pairs) ont le même défaut.
  - Conséquences : HAProxy sans chroot ni baisse de privilèges, ou une
    API runtime admin joignable depuis le réseau, sans authentification.
  - Constaté à la lecture. Le comportement d'HAProxy est celui de son
    manuel (§2.2 et §2.4).
  - CVSS:3.1/AV:N/AC:L/PR:H/UI:N/S:U/C:L/I:H/A:L (6.3).
- **Correctif** :
  - Découper comme le `parse_line` d'HAProxy (guillemets, échappements,
    `${...}`).
  - Refuser les blocs conditionnels dans `global`, ou les évaluer.
  - Refuser un premier mot entre guillemets ou échappé.
  - Un cas de test pour chacun.

## 5. (Moyenne, décision) Un `os:admin` direct emporte les clés des sites servis

**Corrigé (JANUS-2026-031, PR #572, v2026.11.07).** Décision
(2026-11-06) : `os:admin` = toutes les clés du nœud, sauf son
identité. Un `os:admin` contrôle déjà le nœud : sur un nœud sans
Secure Boot, `Upgrade` accepte `insecure_skip_signature_check`, et
faute d'attestation (TPM) la source ne peut pas savoir qu'une
demande de clé vient d'un janusd. Le correctif (b) complet ne tient
donc pas. La clé de la CA du nœud, valable des années, ne part plus
qu'avec un certificat de la CA du nœud (secours, ancien `admin.crt`)
ou de la racine de la flotte elle-même (`janusctl system state
export|import -from ... -identity -kit KIT`). Le reste de l'état
suit un admin qui appelle le nœud, comme le disent maintenant
`secretfiles.go`, `docs/haproxy-files.md` et `docs/internals/trust.md`.
Le chemin `ClusterSet` avec une CA à soi reste ouvert à un admin - il
fixe la liste des membres que vérifie `AcceptMember` (#6) -, cohérent
avec cette décision : un pull de cluster ne porte jamais l'identité.

**Complété (JANUS-2026-033, PR #582, v2026.11.08).** En traitant le #28,
un contournement est apparu : n'importe quel admin peut faire émettre au
nœud un certificat de sa propre CA (`GenerateClientConfiguration`,
jusqu'à un an, aussi sur la page du nœud dans le Controller ;
`LocalCARotate`), qui obtenait alors l'identité et passait `TrustReset`.
Un certificat émis **à la console** - le secours - porte désormais une
marque qu'aucun appel d'API ne pose (OID 2.999.1.2) : l'identité et
`TrustReset` exigent cette marque, ou un certificat de la racine de la
flotte (`-kit`). L'`admin.crt` du premier démarrage des anciens nœuds ne
suffit plus.

- **Où** : `internal/api/system_state.go:25-45` (`transferAllowed`
  refuse seulement `Via != ""`), `internal/api/state_grant.go:161-203`,
  `internal/api/cluster.go:186-218`, `internal/cluster/cluster.go:442-463`.
- **Risque** :
  - Un `os:admin` qui appelle le nœud lui-même peut faire sortir le
    contenu brut de STATE. Cela vaut pour un certificat `janusctl login`
    de 12 h, un certificat obtenu par un jeton admin, un certificat
    scoped admin du domaine `system`, un certificat de secours.
  - Ce contenu comprend :
    - `haproxy/runtime-certs/` et `haproxy/files/` (avec leurs clés) ;
    - `config/acme/` (clé de compte, identifiants DNS, mot de passe du
      proxy) ;
    - `config/logforward/` ;
    - l'anneau des clés de tickets ;
    - avec l'identité, `pki/ca.key`.
  - Trois chemins y mènent :
    - `StateExport` ;
    - `StateExportGrant`, qui signe n'importe quelle CSR P-256 (même
      relayé par le Controller, hors identité) ;
    - `ClusterSet` avec une CA à soi, puis un certificat de membre pour
      sa propre clé (`AcceptMember` ne vérifie pas que le nom est un
      membre, #6), puis `ClusterExport`.
  - La documentation dit le contraire : `internal/api/secretfiles.go:13-20`
    (« No other API returns them either… ») et `docs/haproxy-files.md`
    (« its content never leaves the node »).
  - Conséquence : un identifiant admin de 12 h volé donne les clés des
    sites pour la durée de leurs certificats. Avec les clés de tickets,
    il permet de déchiffrer les sessions TLS 1.2 enregistrées qui les
    ont utilisées.
  - CVSS:3.1/AV:N/AC:L/PR:H/UI:N/S:U/C:H/I:N/A:N (4.9).
- **Correctif**, selon ce qui est décidé :
  - (a) « admin = toutes les clés » : le dire dans `secretfiles.go`,
    `haproxy-files.md` et `docs/internals/trust.md`, et garder le code.
  - (b) Sinon :
    - réserver les classes secrètes et l'identité à un certificat de la
      CA propre du nœud ou à une autorisation (grant) dédiée ;
    - chiffrer l'export pour une clé née dans la mémoire du janusd de
      destination ;
    - n'accepter dans `ClusterSet` qu'une CA de cluster chaînée à la
      racine de la flotte ;
    - corriger `AcceptMember` (#6).

## 6. (Moyenne) Un membre retiré garde son accès de membre

- **Où** : `internal/cluster/cluster.go:442-463` (`AcceptMember`),
  `internal/api/authz.go:260-271`,
  `internal/clusters/engine.go:727-799` (`removeMember`),
  `internal/cluster/peersfile.go:55-58`.
- **Risque** :
  - `AcceptMember` vérifie l'ancre (la CA du cluster) et l'identifiant
    du cluster, puis rend le nom sans chercher s'il figure encore dans
    la composition.
  - Le retrait d'un membre renouvelle l'anneau de tickets mais ne
    révoque pas son certificat (30 jours) et ne change pas la CA.
  - Le listener des pairs accepte tout certificat de la CA, sans CRL.
  - Un membre retiré de force (injoignable, volé, compromis), ou
    quiconque a sa clé (disque réformé, sauvegarde, #1), appelle donc
    encore `ClusterExport` sur les autres membres. Il obtient les clés
    TLS, les identifiants ACME et l'anneau qu'on vient de renouveler.
    C'est l'inverse de ce que promet `docs/cluster.md`.
  - CVSS:3.1/AV:N/AC:H/PR:H/UI:N/S:U/C:H/I:L/A:N (5.0).
- **Correctif** :
  - Refuser un nom absent de la composition.
  - Mieux : épingler la clé publique (SPKI) de chaque membre dans
    `cluster.json` (via `ClusterSet`) et l'exiger.
  - Pour le listener des pairs : une CRL (`crl-file`), ou une nouvelle
    CA après un retrait forcé.

## 7. (Moyenne) Nom d'hôte et domaine DHCP appliqués sans validation

- **Où** :
  - `internal/netmgr/dhcp.go:99-100` (options 12 et 15 brutes) ;
  - `internal/netmgr/netmgr.go:441-465` (`Sethostname`, `resolv.conf`) ;
  - `netmgr.go:145-151` (`Start`) ;
  - `internal/cluster/peersfile.go:36-54` ;
  - `cmd/janusd/main.go:281`.
- **Risque** :
  - Un nom d'hôte configuré passe par `netconfig.ValidateHostname`.
    Celui d'un bail DHCP non.
  - Sur un nœud hors cluster, ce nom va tel quel dans le fichier de
    pairs généré (commentaire, `set-var`, `server`). Ce fichier est
    passé à chaque `haproxy -c` et à chaque démarrage.
  - Un espace, un saut de ligne ou un caractère qu'HAProxy refuse dans
    un nom de pair rend toute validation impossible, et HAProxy ne
    démarre plus.
  - Plus de 64 octets fait échouer `Sethostname` :
    - plus de `resolv.conf` ;
    - au `Start` d'un janusd redémarré qui tient déjà un bail, la
      configuration réseau enregistrée est abandonnée pour les défauts
      (adresses statiques, VLAN et routes perdus pour ce démarrage).
  - Un domaine avec saut de ligne ajoute des `nameserver` avant ceux de
    l'opérateur.
  - La source est la dernière interface DHCP de la configuration, pas
    seulement l'interface principale.
  - L'attaquant est un serveur DHCP pirate sur un VLAN du nœud, ou
    l'administrateur du serveur légitime.
  - CVSS:3.1/AV:A/AC:H/PR:N/UI:N/S:U/C:N/I:L/A:H (5.9).
- **Correctif** :
  - Dans `leaseFromACK`, écarter un nom que `ValidateHostname` refuse,
    et un domaine que le validateur de domaine refuse.
  - Dans `PeersConfig`, se replier sur un nom fixe quand le nom d'hôte
    n'est pas un nom de pair valide.
  - `ResolvConf` refuse toute valeur avec blanc ou caractère de
    contrôle.

## 8. (Moyenne) Les lecteurs du Controller lisent les versions de cluster non masquées

- **Où** : `dashboard/backend/cluster_api.go:40-42`, `:502-526`,
  `internal/clusters/engine.go:250-281`, `internal/clusters/nodes.go:404-412`.
- **Risque** :
  - `GET /api/clusters/{id}/versions/{n}` est ouvert aux lecteurs et
    rend chaque fichier entier : `haproxy.cfg`, `keepalived.conf`
    (`auth_pass`), `bird.conf` (mots de passe de session), ainsi que les
    `vars` des membres.
  - La version 1 d'un cluster créé depuis un nœud est lue en admin, donc
    non masquée.
  - Sur un nœud, un lecteur ne voit ni ces mots de passe
    (JANUS-2026-006) ni les configurations VRRP/BGP (JANUS-2026-005).
    Le Controller les lui rend.
  - CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:L/I:N/A:N (4.3), comme 005 et 006.
- **Correctif** :
  - Pour un appelant sous admin, `haproxy.RedactConfig` sur
    `haproxy.cfg`, dans la vue comme dans les diffs.
  - `keepalived.conf`, `bird.conf` et les `vars` aux opérateurs et
    admins seulement.
  - Un test de route qui vérifie le masquage.

## 9. (Moyenne) Les clés des stick tables sont servies telles quelles aux lecteurs

- **Où** : `internal/rbac/rbac.go:135` (`TableShow` : lecteurs),
  `internal/api/haproxy_runtime.go:60-81`.
- **Risque** :
  - Une table indexée sur un cookie de session, un en-tête
    `Authorization` ou une clé d'API (une persistance courante) donne
    aux lecteurs les jetons des utilisateurs finaux.
  - `TableClear` publie aussi la clé dans l'événement
    `haproxy.table.cleared`, que les lecteurs lisent et qui part vers
    les cibles de logs.
  - C'est la classe de secret que JANUS-2026-006 cache aux lecteurs.
  - Les maps (`MapGet`, lecteurs) sont dans le même cas quand une map
    tient des clés d'API.
  - CVSS:3.1/AV:N/AC:H/PR:L/UI:N/S:U/C:H/I:N/A:N (5.3).
- **Correctif** :
  - Pour un appelant qui ne peut pas appliquer de configuration,
    masquer les clés des tables de type `string`/`binary`, ou réserver
    `TableShow` aux opérateurs.
  - Hacher ou retirer la clé de l'événement.
  - Dire dans `docs/haproxy-config.md` qu'une map lisible des lecteurs
    ne doit pas tenir de secret.

## 10. (Moyenne) Journal d'audit vidable sans authentification, lignes forgées dans le log

- **Où** : `dashboard/backend/auth_handlers.go:735-756`,
  `internal/audit/audit.go:125-126`, `dashboard/backend/logforward.go:89-120`,
  `sso_login.go:109`, `:388`.
- **Risque** :
  - Chaque requête `/api/…` non-GET est enregistrée, authentifiée ou
    non, avec son chemin et sa requête sans limite de longueur.
  - Le fichier tourne à 4 MiB et garde une seule génération. Environ
    150 requêtes anonymes aux chemins de 60 Ko effacent tout
    l'historique. Rien ne limite leur débit.
  - La ligne de log utilise `%s` sur le chemin décodé : un `%0A` écrit
    une ligne forgée dans le log du processus. La transmission des logs
    en fait un enregistrement `controller` indiscernable d'un vrai.
  - Même chose pour le nom de fournisseur SSO de `ssoFail` et pour un
    nom d'utilisateur choisi par l'IdP à l'échec.
  - CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:L/A:N (5.3).
- **Correctif** :
  - `%q` sur chaque champ venu de l'extérieur, comme le fait déjà
    `cspReport`.
  - Chemin et requête enregistrés bornés (512 octets).
  - Entrées sans compte limitées par adresse, puis un compteur
    « ignorées ».

## 11. (Moyenne) Le relais de mise à jour télécharge toute URL avant le contrôle de rôle

- **Où** : `dashboard/backend/internal/nodeproxy/relay.go:59-85`,
  `:145-156`, `internal/nodeproxy/lifecycle.go:91-97`, `:240`.
- **Risque** :
  - La page d'un nœud accepte une mise à jour par `POST` d'opérateur.
    Le relais télécharge l'URL `http(s)://` donnée, en suivant les
    redirections, **avant** que le nœud ne refuse `UploadReleaseFile`
    (admin).
  - Un opérateur (même limité à un autre domaine) fait donc faire au
    Controller des GET vers ses réseaux internes. Le statut et les
    erreurs de connexion reviennent dans le flux NDJSON : un oracle de
    ports et de statuts.
  - Code du 2026-10-01, après la revue du 2026-09-30.
  - CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:C/C:L/I:N/A:N (5.0).
- **Correctif** :
  - Refuser avant tout téléchargement si l'utilisateur n'a pas
    `UploadReleaseFile` et `Upgrade`.
  - Ne pas suivre une redirection vers un autre hôte.

## 12. (Moyenne, décision) Les codes de secours et d'appairage passent par la console relayée

**Corrigé (JANUS-2026-032, PR #571, v2026.11.07).** Décision
(2026-11-06) : masquer les codes pour tous les lecteurs, admins
compris, comme les clés PEM. Celui qui attache un volume de secours
a accès à l'hyperviseur, dont la console montre le code. Le
Controller remplace chaque mot de la forme d'un code (cinq groupes
de quatre symboles, séquences d'échappement comprises) ;
`hack/qemu-rescue-test.sh` rejoue la console réelle de son démarrage
à travers ce filtre.

- **Où** : `dashboard/backend/console.go:11-36` (seuls les blocs PEM
  sont masqués), `machines_api.go:288-296` (console : opérateur),
  `cmd/janusd/pairing.go:151-156`, `motd.go:86-89`.
- **Risque** :
  - janusd écrit le code de la fenêtre de secours, et celui de
    l'appairage, sur la console série.
  - La console d'une machine créée par le Controller est ouverte aux
    opérateurs de la machine (domaine `machines`).
  - `PairingService/Rescue` délivre un `os:admin` de la CA du nœud, que
    `LocalCARotate` rend durable.
  - Un opérateur qui regarde la console au moment d'un démarrage de
    secours peut donc devancer l'admin.
  - Le masquage existe justement pour que le Controller ne relaie
    jamais un identifiant de console. Le code en est le successeur.
  - CVSS:3.1/AV:N/AC:H/PR:L/UI:R/S:U/C:H/I:H/A:N (6.4).
- **Correctif** : masquer aussi les lignes `pairing: code` et
  `rescue: code`, ou réserver la console aux admins. La décision : un
  admin qui fait un secours par la console du Controller doit-il encore
  y lire le code ?

## 13. (Moyenne) Le hook du plugin autorise tout exécutable nommé `janusctl`

- **Où** : `cmd/janusctl/effect_hook.go:235-237` (`isJanusctl` =
  `path.Base(...) == "janusctl"`), `:312-316`, `:609-630`.
- **Risque** :
  - Avec l'option `allow_reads`, `./janusctl system info` est classé
    lecture et répondu `allow` : Claude Code ne demande rien.
  - Un dépôt cloné qui contient un exécutable `janusctl`, ou un README
    qui y pousse, fait exécuter ce programme avec les droits de
    l'utilisateur.
  - CVSS:3.1/AV:L/AC:H/PR:N/UI:R/S:U/C:H/I:H/A:N (6.3).
- **Correctif** :
  - Ne répondre `allow` que si le mot de tête est exactement `janusctl`
    et se résout au binaire du hook (`exec.LookPath` contre
    `os.Executable`).
  - Garder la reconnaissance par nom de base pour `ask`.

## 14. (Moyenne, plausible) Limite de débit des builds fondée sur un `X-Forwarded-For` usurpable

- **Où** : `site/backend/api.go:62`, `:84-97`, `:142-148`.
- **Risque** :
  - `clientIP` prend la **première** ligne `X-Forwarded-For`. HAProxy
    ajoute la sienne après celle du client : c'est donc la valeur du
    client qui compte.
  - La clé `prebuild` (le budget des pré-builds de release) est une
    chaîne comme une adresse. Un visiteur qui l'envoie, ou qui fait
    tourner de fausses adresses, contourne la limite par adresse et
    vide le budget des pré-builds : les nœuds aux schémas personnalisés
    attendent leur mise à jour de sécurité, et chaque build coûte du
    temps de runner.
  - Dépend du HAProxy de façade (hors dépôt) : à vérifier.
  - CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:L (5.3).
- **Correctif** :
  - Prendre la dernière valeur, et seulement quand `RemoteAddr` est la
    façade.
  - Passer « pré-build » en booléen, jamais comme une adresse.

## 15 à 27. Constats faibles

Tous corrigés dans la release v2026.11.05-2.

- **15. Un paquet ICMP forgé fait tomber janusd pendant un ping**
  (`internal/probe/icmp.go:155-156`).
  - L'heuristique qui retire un en-tête IPv4 s'applique à un message
    dont x/net (v0.62.0, vérifié) a déjà retiré l'en-tête.
  - Un message ICMP de type 0x40-0x4F long de 20 à 59 octets donne un
    découpage hors bornes, donc un panic. janusd n'a pas
    d'intercepteur de récupération, et le socket IPv4 n'a pas de filtre
    ICMP.
  - janusd redémarre, keepalived/BIRD/Consul aussi : la VIP bouge, les
    routes BGP sont retirées.
  - Correctif : supprimer l'heuristique, poser `ICMP_FILTER`, ajouter un
    intercepteur qui récupère les panics.
  - **Corrigé** : JANUS-2026-019, PR #555.
- **16. Un import d'état pose un `haproxy.cfg` jamais vérifié au
  démarrage** (`internal/statexfer` `Stage`,
  `internal/haproxy/manager.go:316`).
  - `Boot` n'appelle pas `Policy.Check`. Un `StateImport` d'admin peut
    donc installer durablement une configuration sans `uid`/`chroot` ou
    avec un second `stats socket`.
  - Correctif : vérifier dans `Stage` et dans `Boot`, avec repli sur la
    configuration d'amorçage.
  - **Corrigé** : JANUS-2026-028, PR #564.
- **17. N'importe qui sur `:9506` ferme la fenêtre d'appairage ou de
  secours** (`internal/pairing/window.go:61-80`).
  - Cinq échecs, comptés par fenêtre et non par source, la ferment
    jusqu'au redémarrage. `Rescue` sur une fenêtre d'appairage est
    audité sans compter d'échec, donc l'audit (2000 entrées) se vide.
  - Correctif : compter par adresse ; après cinq échecs, tirer un
    nouveau code après une pause.
  - **Corrigé** : JANUS-2026-029, PR #565.
- **18. `firewall: skip` dure tout le démarrage**
  (`cmd/janusd/main.go:241-247`).
  - Rien ne remet le jeu de règles quand la fenêtre de secours se
    ferme.
  - Correctif : `fwMgr.Boot()` à la fermeture de la fenêtre.
  - **Corrigé** : JANUS-2026-030, PR #565.
- **19. Un secret « en écriture seule » suit un changement d'URL ou de
  confiance TLS.**
  - Cas trouvés :
    - les identifiants des cibles de logs gardés quand seule la
      confiance TLS change (`internal/logship/config.go:275-294`) ;
    - le mot de passe RouterOS gardé quand l'URL change
      (`internal/clusters/balancer.go:503-522`) ;
    - les réglages d'un fournisseur DNS ACME du Controller ;
    - le jeton Consul du test ;
    - le secret client SSO quand l'`issuer` change.
  - Un admin (ou un admin d'un seul label pour les logs) envoie ainsi un
    secret qu'il ne peut pas lire au serveur de son choix.
  - Correctif : redemander le secret dès qu'un champ non secret de la
    même entrée change.
  - **Corrigé** : JANUS-2026-026, PR #563.
- **20. `Logs` d'un démarrage précédent**
  (`internal/api/system_logs.go:63-68`, `:162-183`).
  - `id: "kernel"` rend le dmesg stocké à un opérateur (`Dmesg` est
    admin).
  - `tail_lines` n'a pas de plafond : jusqu'à un démarrage entier de
    LOGS en mémoire, et l'OOM frappe HAProxy avant janusd.
  - Correctif : liste blanche des services, plafond de lignes et
    d'octets.
  - **Corrigé** : JANUS-2026-023, PR #559.
- **21. Fuites vers les lecteurs.**
  - L'événement `probe.run` publie la cible, avec ses identifiants
    `user:pass@` (`internal/api/probe.go:47`).
  - Le masquage laisse passer `tcp-check send`, les ACL sur
    `http_auth_bearer`, `req.cook`, `url_param` et les `hdr` de
    `http-request return` (`internal/haproxy/redact.go:74-121`).
  - `ConfigHistory`/`ConfigVersion` rendent l'empreinte non masquée
    quand le texte manque (`haproxy.go:139`, `:205`), ce qui rouvre
    l'oracle que 006 a fermé.
  - **Corrigé** : JANUS-2026-023, PR #559.
- **22. Sessions et certificats qui survivent à leur compte.**
  - `DeleteUser` garde les sessions, et un nom réutilisé les ranime
    (plausible) (`internal/auth/auth.go:762-779`).
  - Une approbation navigateur/appareil de `janusctl login` émet un
    certificat de 12 h avec le rôle du moment de l'approbation
    (`cli_browser.go:188`, `:267`, `:377`, `:408`).
  - Désactiver un fournisseur SSO laisse marcher les jetons d'API et
    les clés SSH de ses comptes (`sso_settings.go:87-89`).
  - **Corrigé** : JANUS-2026-020, PR #556.
- **23. La table des connexions SSO en attente se remplit** depuis un
  /64 IPv6 ou ~90 adresses IPv4 (`internal/sso/logins.go:28`,
  plausible). Les connexions SSO répondent alors « occupé » pendant
  10 minutes ; le mot de passe n'est pas touché.
  - **Corrigé** : JANUS-2026-024, PR #560.
- **24. Clé SSH libvirt et jeton Proxmox en clair** dans
  `hypervisors/<id>/` (`internal/hypervisor/store.go:322-333`).
  - Tous les autres secrets sont scellés avec la clé maîtresse. La
    promesse « une copie des données ne contient rien d'utilisable »
    ne tient pas pour ces deux fichiers.
  - Correctif : les sceller, avec migration au chargement.
  - **Corrigé** : JANUS-2026-021, PR #557.
- **25. Hook du plugin, suite.**
  - Une commande dans une variable (`X='janusctl …'; eval "$X"`,
    `bash -c "$X"`, `$J system $R`) n'obtient aucune décision. Le
    tableau de `docs/guide/claude-code.md` dit pourtant qu'elle
    demande.
  - Effets mal classés (`cmd/janusctl/effects.go`) :
    - `system pcap` et `system support` sans `-o` sont auto-autorisés
      alors qu'ils écrivent ou exposent du trafic ;
    - `network vrrp apply`, `network bgp apply`, `haproxy server-state`,
      `haproxy session-kill` et `login -kit` répondent à la définition
      de « critique » du fichier lui-même.
  - **Corrigé** : JANUS-2026-025, PR #561.
- **26. Release et pré-release publiables depuis n'importe quelle
  branche** (`image-build.yml:1308-1313`, `:1578`, `release-promote.yml`).
  - Une pré-release lancée sur une branche pousse `latest` sur Docker
    Hub. Une release non-maintenance d'une autre branche devient la
    dernière partout.
  - Correctif : exiger `refs/heads/main` (et `release/*` pour la
    maintenance), et un Environment GitHub que le compte du site ne
    peut pas utiliser.
  - **Corrigé** : JANUS-2026-022, PR #558.
- **27. Plafonds manquants.**
  - `ClusterPull` garde tout l'export en mémoire, sans plafond total,
    et tronque en silence une entrée à 4 MiB
    (`internal/api/cluster_pull.go:185-210`).
  - Le site interroge GitHub pour chaque version inconnue sans mettre
    l'absence en cache (`site/backend/github.go:162-187`). Une boucle
    anonyme épuise le quota du jeton.
  - **Corrigé** : JANUS-2026-027, PR #562.

## 28. Points de durcissement (info)

Points sans risque direct, relevés pour la défense en profondeur.

**Traités (PR #566, v2026.11.05-2)**, sans enregistrement - ce ne sont
pas des vulnérabilités divulguées :
- `/etc/.state/import/` (l'état mis en scène, clés d'identité d'un
  autre nœud comprises) n'était pas dans `SecretPaths` : il y est ;
- `janusctl cluster checkout` écrivait en 0644 : 0600 ;
- le nom de fichier par défaut du bundle de support venait du nom
  d'hôte du nœud tel quel : il est assaini ;
- en standby, `GET /api/ha` (ouvert à tous) citait l'erreur brute du
  pull, qui peut nommer l'adresse de l'actif : elle reste dans
  `dashboardd ha status`, sur l'hôte.

**Relus et gardés** (choix de conception ou notes d'infrastructure) :
- l'exporter (`:10056`) lit le socket stats à chaque scrape, sans cache
  - le même socket que les garde-fous VRRP/BGP ;
- le flux appareil de `janusctl login` préremplit le code sur la page
  (RFC 8628 §5.4) ;
- le Controller détient les clés des CA de ses clusters : un Controller
  compromis signe un membre (dit dans `docs/cluster.md`) ;
- le miroir parle au site en HTTP clair, avec son jeton, sur le réseau
  interne ;
- `image-build.yml` n'a pas de bloc `permissions` au niveau du
  workflow : ses jobs de test prennent les droits par défaut du dépôt,
  en lecture seule ; seul le job de publication fixe les siens.

**Traités dans v2026.11.08** (PR #583 à #586), sans enregistrement sauf
le premier :
- l'heure du certificat de secours n'était pas une borne
  (`LocalCARotate`, `GenerateClientConfiguration` donnaient un certificat
  durable de la CA du nœud) : JANUS-2026-033, ci-dessus (#5) ;
- le test de connectivité joignait `127.0.0.1` et `169.254.169.254` : un
  opérateur limité au domaine `network` gagnait ce qu'`ApplyConfig` donne
  aux opérateurs HAProxy. Ces destinations sont désormais réservées à qui
  peut changer la configuration HAProxy, vérifiées sur l'adresse que le
  socket joint (#584) ;
- un standby retiré gardait sa dernière copie scellée : l'actif lui
  répond 410, et il l'efface à son pull suivant ; un standby avertit
  aussi d'une clé maîtresse dans son répertoire de données (#585) ;
- le MFA « du fournisseur » sans ACR/AMR comptait toute connexion SSO
  comme second facteur, y compris pour un compte lié qui a son mot de
  passe ou son facteur : un tel compte garde le sien (#585) ;
- le limiteur de `/csp-report` était global : il est désormais par
  source, dans le plafond global (#585) ;
- l'empreinte de 59 bits du code protégeait une fenêtre d'appairage sans
  durée de vie : une fenêtre ouverte change de certificat et de code
  toutes les heures (#586) ;
- `findRescue` analysait les volumes dans PID 1 sans `recover()` : une
  panique y saute l'étape, le démarrage continue (#584) ;
- le CN `janus-controller` suffisait pour être affiché comme le
  Controller dans les clients d'un nœud : seul le rôle `janus:controller`
  compte (#584) ;
- la TUI filtrait C0 mais pas C1, et la carte des flux contournait le
  filtre : chaque cellule passe par le filtre, C1 et contrôles
  bidirectionnels compris (#583).

**Corrigé sur `main`, dans la release qui suit v2026.11.08** (PR #588) :
la politique Consul `janus-nodes` permettait à tout nœud géré
d'enregistrer un service `haproxy-*` d'un autre cluster. Les services
HAProxy d'un nœud portent désormais le nom de son cluster (ou le sien
s'il est seul), `haproxy-<scope>-...`, et son jeton n'écrit que ceux-là
(une politique par scope) ; un nœud d'une release antérieure garde
l'ancien droit jusqu'à sa mise à jour.

## Vérifié sans constat

- **Appairage** :
  - secret de 36 bits tiré par `crypto/rand`, comparé en temps
    constant ;
  - code seulement sur stderr et le motd (ni log, ni LOGS, ni
    événements, ni audit) ;
  - côté client, feuille, signature et CA vérifiées avant tout envoi ;
  - service sur son propre port, pseudo-rôle impossible à émettre.
- **Secours** : volume trouvé au démarrage seulement, disque de
  démarrage exclu, champs inconnus refusés, certificat d'une heure,
  `Used` persistant.
- **Autorisation** :
  - certificats de membre et de transfert aiguillés avant les rôles ;
  - autorisation de transfert à usage unique ;
  - certificat scoped à un seul O ;
  - rôles agissants du Controller limités aux rôles humains.
- **Clusters** :
  - `ClusterExport` limité à `O=janus:cluster`, sur la CA et
    l'identifiant de ce cluster ;
  - SNI du pull vérifié ;
  - contenu tiré repassé par les mêmes validateurs ;
  - listener des pairs en TLS `verify required` (membres) ou sur
    `127.0.0.1` (seul) ;
  - verrous : la clé n'est acceptée que du certificat du Controller ;
  - gabarits en `text/template`, `missingkey=error`, sans fonction.
- **Transfert d'état** :
  - chemins nettoyés, `..` et absolus refusés ;
  - liste de classes revérifiée deux fois ;
  - SHA-256 et tailles vérifiés ;
  - marqueur `ready` écrit en dernier ;
  - export janusctl en 0600 `O_EXCL`.
- **Client DHCP** : trames bornées, filtre BPF, durées bornées, MTU
  ≥ 576, adresses analysées comme IP, étiquette `IFA_PROTO`.
- **Sonde** : opérateurs, 4 à la fois, 60 s ; HTTP sans proxy ni
  redirection ; ICMPv6 filtré et borné.
- **ACME** (nœud) :
  - mot de passe du proxy masqué et repris pour la même URL seulement ;
  - certificat du relais sans rôle ;
  - clé commandée née sur le nœud et dans `SecretPaths` ;
  - chaîne installée vérifiée (noms, clé, type).
- **API runtime HAProxy** : arguments sans caractère de contrôle et
  échappés ; réponses plafonnées.
- **Consul** : jetons masqués, `consul validate` à argv fixe, client
  HTTPS plafonné.
- **SELinux et noyau** : aucune règle nouvelle n'élargit `haproxy_t` ni
  un domaine d'extension ; `NF_DUP` n'est atteignable que par des RPC
  admin.
- **SSO** :
  - état lié au cookie et à usage unique, nonce et PKCE S256 ;
  - jeton d'identité vérifié (émetteur, audience, `azp`, `at_hash`,
    algorithmes asymétriques) ;
  - liaison par émetteur et sujet, jamais par nom ou adresse ;
  - pas de redirection ouverte ;
  - secret client scellé.
- **Routes du Controller** : toutes les nouvelles routes `/api/` ont
  leur garde. Seules sont sans authentification, par conception, celles
  du SSO, `/api/ha/active`, `/csp-report` et `/node-app/`. CSP
  appliquée, corps plafonnés.
- **Standby et témoin** :
  - jeton d'appairage haché, valable 1 h, limité ;
  - épinglage mutuel, TLS 1.3, porteur comparé en temps constant ;
  - copie scellée et vérifiée, `ha/` hors des sauvegardes ;
  - époques croissantes ;
  - témoin signé et vérifié, écritures conditionnelles.
- **Relais ACME du Controller** : client vérifié à la poignée de main
  et à chaque requête, CONNECT seul vers une liste exacte, limites par
  nœud.
- **Restauration** : fichiers réguliers seulement, préfixe et `../`
  contrôlés, `ha/` ignoré, `resealHA` sur les deux chemins.
- **Télémétrie** : désactivée jusqu'à l'accord d'un admin, comptes
  seulement, identifiant aléatoire.
- **Hyperviseurs** : noms et réseaux validés avant les gabarits
  shell/XML, Proxmox épinglé.
- **CI** :
  - entrées passées par `env:` et validées ;
  - pas de `pull_request_target` ;
  - `ci.yml` sur des runners GitHub en `contents: read` ;
  - `FROM` en `tag@digest`, téléchargements épinglés par SHA-256 ;
  - publication apt en commande forcée avec clé d'hôte épinglée ;
  - `hack/releases publish` en brouillon jusqu'au bout ;
  - releases de maintenance bien bornées.
- **Analyseur du hook** : guillemets, `$'…'`, heredocs, `$(…)`, `sh -c`,
  `xargs`/`find`/`ssh` traités ; `readsOnly` exige une lecture directe
  sans affectation ni substitution.

## Non couverts

- Les dépendances (go-diskfs sur un volume malformé, la bibliothèque
  DHCP, lego) n'ont été lues qu'à leur frontière.
- Le récepteur de télémétrie (hors dépôt).
- Les réglages du dépôt et des déploiements invisibles d'ici :
  - le traitement de `X-Forwarded-For` par la façade du site (#14) ;
  - le transport miroir ↔ site ;
  - les permissions par défaut des workflows et l'approbation des PR de
    forks (#3) ;
  - la protection de `SECUREBOOT_SIGNING_KEY` par un Environment.
- `timesync` face à un NTP fourni par DHCP.
- Le détail de `cluster_scale`, `cluster_placement` et
  `cluster_recycle`.
