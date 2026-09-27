# Provisionner un nouveau node auto-enregistré

Deux méthodes pour créer un node Janus qui s'auto-enregistre auprès d'un
Controller déjà en place (voir `CLAUDE.md`'s "Point 2 suite" pour le
design du self-registration).

- **Méthode 1 - image partagée + `seed-controller`** : recommandée dès
  qu'on provisionne plusieurs nodes pour le même environnement/Controller
  (le cas visé par le futur site compagnon). Une seule image générique,
  jamais reconstruite par node.
- **Méthode 2 - `Install` par node** : le chemin complet, utile pour un
  seul node ponctuel, ou si le rootfs lui-même doit être différent
  d'un node à l'autre (versions/contenu différents - `Install` écrit le
  rootfs en plus de la config).

## Méthode 1 : image partagée + `seed-controller` (recommandée)

Principe : `image/kvm-proxmox/assemble.sh` (ou `make proxmox-image`)
produit une image générique, avec une partition STATE déjà présente mais
vide - **exactement la même pour tous les nodes**. `janusctl image
seed-controller` écrit juste `controller_address`/`controller_ca_cert`
dans cette partition, **sans repartitionner, sans réécrire le rootfs, et
sans lancer aucun `janusd`/appel gRPC** (voir `internal/diskseed`). Une
seule commande locale par node, aucun serveur à faire tourner.

C'est l'équivalent Janus de l'option **"Embedded machine configuration"**
de Talos Image Factory (`factory.talos.dev`) - injecter la config
directement dans l'image plutôt que de la livrer séparément au boot.

### Prérequis

- L'image générique déjà construite : `make proxmox-image` (ou
  `image/disk/assemble.sh` pour le raw avant conversion qcow2).
- `bin/janusctl` à jour (`make build`).
- Le certificat CA du Controller (non sensible, récupérable sans
  identifiants) :

```sh
echo | openssl s_client -connect <CONTROLLER_HOST>:8443 \
    -servername <CONTROLLER_HOST> 2>/dev/null \
  | openssl x509 -outform PEM > controller-ca.crt
```

### Pour un disque raw

```sh
cp build/disk.img mon-node.img   # ou toute copie de l'image générique

./bin/janusctl image seed-controller \
  -controller-address <CONTROLLER_HOST>:8443 \
  -controller-ca controller-ca.crt \
  mon-node.img
```

Un second appel sur le même disque est **refusé** (pas d'écrasement
silencieux) - repartir d'une copie fraîche de l'image générique si besoin
de changer de Controller.

### Pour un qcow2 (ce que produit `make proxmox-image`)

`go-diskfs` (utilisé par `seed-controller`) ne sait pas lire le qcow2
directement - convertir en raw, seed, reconvertir :

```sh
qemu-img convert -O raw mon-node.qcow2 mon-node.img
./bin/janusctl image seed-controller \
  -controller-address <CONTROLLER_HOST>:8443 \
  -controller-ca controller-ca.crt \
  mon-node.img
qemu-img convert -O qcow2 -c mon-node.img mon-node.qcow2
rm mon-node.img
```

### Déployer et vérifier

Transférer/attacher `mon-node.qcow2` (ou `.img`) comme d'habitude
(`qm importdisk`, upload direct, etc.), démarrer la VM, puis suivre les
logs du Controller :

```sh
ssh root@<CONTROLLER_HOST> "docker logs janus-controller -f"
```

Attendre `node self-registered: ...`, puis approuver dans l'UI (ou via
`GET /api/pending`).

**À l'échelle** : la même image seedée une fois par environnement peut
être clonée (linked-clone Proxmox ou équivalent) pour autant de nodes que
nécessaire - `seed-controller` ne tourne qu'une fois par
environnement/Controller, pas une fois par node.

## Méthode 2 : `Install` par node (chemin complet)

C'est le chemin suivi par `hack/lifecycle-install-test.sh` et par un
déploiement réel (VM Proxmox) - plus lourd, mais nécessaire si le rootfs
lui-même (pas juste la config Controller) doit différer par node.

`LifecycleService.Install` a besoin d'un `janusd` déjà en cours
d'exécution pour servir l'appel RPC - mais le node qu'on installe
n'existe pas encore. On fait donc tourner un `janusd` **natif**,
temporaire, juste le temps de l'appel, sur n'importe quelle machine Linux
avec `sudo` (le poste de build fait très bien l'affaire) - ce `janusd`
natif écrit directement sur un fichier disque, qu'on transfère ensuite
vers l'hyperviseur cible.

### Prérequis

- Un build à jour : `make build rootfs-build` (produit `bin/janusctl`,
  `build/janusd`, `build/bzImage`, `build/rootfs/{rootfs.squashfs,
  rootfs.verity,rootfs.roothash}`).
- Un Controller (`dashboardd`) déjà en cours d'exécution et joignable en
  HTTPS sur son port d'enregistrement (`:8443` par défaut).
- `sudo` sur la machine où cette procédure s'exécute (pour le `chroot()`
  de `janusd`/`haproxy`).

### 1. Assembler un release bundle

```sh
mkdir -p /tmp/provision/bundle
image/release/assemble.sh /tmp/provision/bundle build/bzImage build/rootfs
```

Produit `rootfs.squashfs`/`rootfs.verity`/`uki-a.efi`/`uki-b.efi` dans
`/tmp/provision/bundle`.

### 2. Récupérer le certificat CA du Controller

```sh
echo | openssl s_client -connect <CONTROLLER_HOST>:8443 \
    -servername <CONTROLLER_HOST> 2>/dev/null \
  | openssl x509 -outform PEM > /tmp/provision/controller-ca.crt
```

### 3. Préparer un disque cible vierge

```sh
truncate -s 2G /tmp/provision/disk.img
```

### 4. Lancer un `janusd` natif temporaire

```sh
sudo ./bin/janusd -addr 127.0.0.1:17500 \
  -pki-dir /tmp/provision/native-pki \
  -haproxy-binary /nonexistent \
  -haproxy-config /tmp/provision/haproxy.cfg \
  -haproxy-pid /tmp/provision/haproxy.pid \
  -haproxy-stats-socket /tmp/provision/haproxy.sock \
  -haproxy-chroot-dir /tmp/provision/haproxy-chroot \
  > /tmp/provision/native-janusd.log 2>&1 &
```

(`-haproxy-binary /nonexistent` : on n'a pas besoin d'un vrai HAProxy
pour servir `Install`, l'échec de démarrage est non-fatal.) Attendre
2-3s, puis vérifier que la ligne `janusd ... listening on
127.0.0.1:17500` apparaît dans le log.

### 5. Appeler `Install`

```sh
sudo ./bin/janusctl \
  -ca /tmp/provision/native-pki/ca.crt \
  -cert /tmp/provision/native-pki/admin.crt \
  -key /tmp/provision/native-pki/admin.key \
  -endpoint 127.0.0.1:17500 \
  lifecycle install \
  -controller-address <CONTROLLER_HOST>:8443 \
  -controller-ca /tmp/provision/controller-ca.crt \
  /tmp/provision/disk.img /tmp/provision/bundle
```

**Utiliser le hostname réel du Controller ici, pas son IP** - le node
sait maintenant écrire son propre `/etc/resolv.conf` au boot (voir
`rootfs/init/main.go`'s `writeResolvConf`, corrigé le 2026-09-27), donc
plus besoin de contourner par IP.

Doit terminer par `[done 100%] installed to ... (slot A active)`.

### 6. Vérifier le disque (sanity check)

```sh
sudo sgdisk -p /tmp/provision/disk.img
```

Aucun warning attendu (en particulier pas de "Secondary partition table
overlaps..."). Arrêter et nettoyer le `janusd` natif :

```sh
sudo pkill -f 'bin/janusd.*native-pki'
sudo rm -rf /tmp/provision/native-pki
```

### 7. Transférer le disque vers l'hyperviseur cible

Exemple pour un LV Proxmox déjà créé (`vm-<VMID>-disk-N`, même taille
que `disk.img`) :

```sh
ssh root@<PROXMOX_HOST> "dd of=/dev/pve/vm-<VMID>-disk-N bs=4M conv=fsync" \
  < /tmp/provision/disk.img
```

Vérifier après coup côté hyperviseur :

```sh
ssh root@<PROXMOX_HOST> "sgdisk -p /dev/pve/vm-<VMID>-disk-N"
```

### 8. Démarrer la VM

Config Proxmox type (voir aussi `image/kvm-proxmox/README.md`) :
BIOS OVMF, `machine: q35`, `boot: order=virtio0`, `serial0: socket` +
`vga: serial0` (pas de VGA/framebuffer sur ce rootfs - la console noVNC
resterait vide sans ça).

```sh
ssh root@<PROXMOX_HOST> "qm start <VMID>"
```

### 9. Confirmer l'auto-enregistrement

Suivre les logs du Controller :

```sh
ssh root@<CONTROLLER_HOST> "docker logs janus-controller -f"
```

Attendre la ligne :

```
node self-registered: <node-ip> (<node-ip>:9505), awaiting approval
```

Puis approuver le node dans l'UI du Controller (section "En attente" /
"Pending") - il apparaît aussi via `GET /api/pending` si besoin de
scripter l'approbation.

### Nettoyage

```sh
rm -rf /tmp/provision
```

## Récupérer les identifiants PKI d'un node (les deux méthodes)

Utile pour un accès direct `janusctl`/navigateur (vue par-node) en plus
du Controller. Les identifiants sont sur la partition STATE (partition 6
du disque, format ext4) - extraction sans montage, via `debugfs` :

```sh
ssh root@<PROXMOX_HOST> "sgdisk -i 6 /dev/pve/vm-<VMID>-disk-N" \
  | grep -E 'First sector|Partition size'
# -> noter START_SECTOR et SECTOR_COUNT

ssh root@<PROXMOX_HOST> "
  dd if=/dev/pve/vm-<VMID>-disk-N of=/tmp/state.img bs=512 \
     skip=<START_SECTOR> count=<SECTOR_COUNT> status=none
  debugfs -R 'dump pki/ca.crt /tmp/node-ca.crt' /tmp/state.img
  debugfs -R 'dump pki/admin.crt /tmp/node-admin.crt' /tmp/state.img
  debugfs -R 'dump pki/admin.key /tmp/node-admin.key' /tmp/state.img
"
scp root@<PROXMOX_HOST>:/tmp/node-{ca.crt,admin.crt,admin.key} /tmp/provision/
```

Construire un `.pfx` sans mot de passe (le même fichier qu'on importe
dans le navigateur pour la vue par-node, ou dans le formulaire d'ajout
du Controller) :

```sh
openssl pkcs12 -export \
  -inkey /tmp/provision/node-admin.key \
  -in /tmp/provision/node-admin.crt \
  -certfile /tmp/provision/node-ca.crt \
  -passout pass: \
  -out /tmp/provision/node-admin.pfx
```

**Attention** : la PKI d'un node est générée à son premier vrai boot, pas
à `Install`/`seed-controller` - un `.pfx` déjà extrait devient obsolète
si le disque est reconstruit ou re-bootera "à zéro" (nouvelle PKI); le
régénérer depuis le disque *actuellement démarré*.

```sh
ssh root@<PROXMOX_HOST> "rm -f /tmp/state.img /tmp/node-ca.crt /tmp/node-admin.crt /tmp/node-admin.key"
```
