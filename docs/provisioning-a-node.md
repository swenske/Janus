# Provisionner un nouveau node auto-enregistré

Quatre méthodes pour créer un node Janus qui s'auto-enregistre auprès
d'un Controller déjà en place : au premier démarrage, le node envoie au
Controller un certificat de service qu'il vient de créer, et il apparaît
dans les approbations en attente.

- **Méthode 1 - image partagée + `seed-controller`** : recommandée pour
  plusieurs nodes qui partagent le même environnement/Controller. Une
  seule image générique, seedée une fois, jamais reconstruite par node.
- **Méthode 2 - `Install` par node** : le chemin complet, utile pour un
  seul node ponctuel, ou si le rootfs lui-même doit être différent
  d'un node à l'autre (versions/contenu différents - `Install` écrit le
  rootfs en plus de la config).
- **Méthode 3 - volume NoCloud (`cidata`)** : recommandée quand
  Terraform (ou tout autre outil d'IaC) pilote déjà le provisioning -
  l'image reste totalement générique et partagée (jamais touchée), et
  c'est un petit volume à part, généré par le même outil qui génère déjà
  du cloud-init pour vos autres VMs, qui porte la config. Pas de commande
  `janusctl` à lancer du tout côté node.
- **Méthode 4 - l'ISO d'installation** : pour une machine physique (clé
  USB) ou une VM installée depuis un média. L'ISO démarre un Janus
  temporaire qui installe le disque de la machine, déjà configuré avec le
  Controller.

Les adresse et certificat CA du Controller se trouvent dans son panneau
**Provision a new node** (ou `GET /api/controller-info`).

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

## Méthode 3 : volume NoCloud (`cidata`)

Principe : au boot, `rootfs/init` scanne les disques attachés - virtio,
SCSI/SATA/USB, NVMe, et les lecteurs CD-ROM (hors son propre disque de
boot) - à la recherche d'un volume (ISO9660 ou vfat) étiqueté
`cidata`/`CIDATA` - exactement la convention "NoCloud" de
cloud-init, celle que Talos Linux lui-même réutilise pour sa propre
config machine. **Le contenu n'est pas du vrai cloud-init** (`#cloud-
config`, `write_files`, etc.) - juste du JSON minimal Janus
(`controller_address`/`controller_ca_cert`), dans un fichier `user-data`
à la racine du volume. N'importe quel outil qui sait déjà générer un
disque cloud-init pour vos autres VMs (provider Terraform
libvirt/Proxmox/OpenStack, `cloud-localds`, etc.) sait déjà produire ce
volume - seul le contenu change.

L'image du node reste **totalement générique et partagée** - rien n'est
jamais écrit dedans pour ce mécanisme, contrairement aux méthodes 1/2.

### Construire le volume à la main (exemple avec `mtools`)

```sh
truncate -s 1M cidata.img
mkfs.vfat -F 12 -n cidata cidata.img

cat > user-data <<EOF
{"controller_address":"<CONTROLLER_HOST>:8443","controller_ca_cert":"$(python3 -c 'import json,sys; print(json.dumps(open(sys.argv[1]).read()))' controller-ca.crt | sed 's/^"//;s/"$//')"}
EOF
mcopy -i cidata.img user-data ::user-data
```

Attacher `cidata.img` comme un disque supplémentaire à la VM (en plus du
disque système, qui reste l'image générique inchangée) - ou un ISO9660
`cidata` comme CD-ROM, ce que fait le lecteur cloud-init de Proxmox
(`xorriso -as mkisofs -V cidata -J -r -o cidata.iso <dossier avec
user-data>`) - puis
démarrer normalement - le node lit le volume, écrit
`controller/address`/`controller/ca.crt` sur sa propre partition STATE,
et s'auto-enregistre.

### Récupération à distance (`seedfrom`)

Au lieu d'un `user-data` complet en local, le volume peut ne contenir
qu'un `meta-data` pointant vers une URL (vraie fonctionnalité cloud-init
NoCloud, reprise telle quelle) :

```json
{"seedfrom": "https://exemple.interne/janus/user-data"}
```

Trois modes de confiance possibles pour cette récupération, choisis
automatiquement selon ce qui est fourni (voir `internal/nocloud` pour le
détail) :

| `meta-data` contient... | URL | Comportement |
|---|---|---|
| `seedfrom_ca_cert` (PEM) | doit être `https://` | vérifié *uniquement* contre ce CA (extension propre à Janus, pas du cloud-init standard) |
| rien de plus | `https://` | vérifié contre le trust store système (comportement HTTPS standard) |
| rien de plus | `http://` | aucune vérification, en clair (façon PXE `talos.config=`) |

### Terraform (exemple conceptuel)

```hcl
data "cloudinit_config" "janus_seed" {
  gzip          = false
  base64        = false
  part {
    content_type = "application/json"
    content      = jsonencode({
      controller_address  = "controller.example.com:8443"
      controller_ca_cert  = file("controller-ca.crt")
    })
  }
}
# ... attacher le résultat comme un disque cloud-init au provider utilisé
# (libvirt_cloudinit_disk, proxmox cicustom, etc.) - le contenu ci-dessus
# devient le user-data du volume cidata, peu importe le provider.
```

## Méthode 4 : l'ISO d'installation

Principe : l'ISO (`janus.iso` des releases, ou celle d'un schéma de
[janus.sw-servers.net](https://janus.sw-servers.net)) démarre un Janus
**temporaire**, sans partition STATE, qui embarque le bundle signé de sa
release (`/etc/janus/release`). Depuis votre poste, `janusctl lifecycle
install` lui fait installer le disque de la machine, avec l'adresse et le
CA du Controller. C'est le disque installé qui s'annonce au Controller, à
son premier démarrage - jamais l'ISO.

### 1. Démarrer sur l'ISO

- **Machine physique** : copier l'ISO sur une clé USB (`dd
  if=janus.iso of=/dev/sdX bs=4M conv=fsync`) et démarrer dessus en
  UEFI, Secure Boot désactivé (ou avec le certificat de Janus enrôlé).
- **VM** : attacher l'ISO comme **disque** (Proxmox : `qm importdisk
  <vmid> janus.iso <stockage>`, puis démarrer sur ce disque), avec le
  disque cible vierge à côté.
- **Pas comme CD/DVD** (lecteur optique, lecteur CD-ROM d'une VM, média
  virtuel « CD » d'un iDRAC/iLO) : la racine de l'ISO est une partition
  de son image disque, qu'un lecteur optique n'expose pas. Les médias
  virtuels qui présentent l'image comme un disque amovible
  (« removable disk », « USB key ») fonctionnent.

La console - série, et écran - affiche une fois le certificat CA, le
certificat admin et la clé de cette instance temporaire : copiez-les
(`ca.crt`, `admin.crt`, `admin.key`). Ils ne servent qu'à piloter
l'installation.

### 2. Trouver le disque cible

```sh
CTL="janusctl -endpoint <IP-de-la-machine>:9505 -ca ca.crt -cert admin.crt -key admin.key"
$CTL system mounts | grep /etc/janus/release   # le disque de l'ISO (ex. /dev/sdb1 -> /dev/sdb)
$CTL system info                               # les disques vus par le noyau
```

Le disque cible est l'autre : `/dev/sda`, `/dev/nvme0n1`... `Install`
refuse le disque d'où l'ISO a démarré, et un disque qui porte déjà un
Janus.

### 3. Installer

```sh
$CTL lifecycle install \
  -controller-address <CONTROLLER_HOST>:8443 -controller-ca controller-ca.crt \
  [-network-config net.json] \
  -sha256 "$(curl -sL https://github.com/swenske/Janus/releases/download/<VERSION>/rootfs.squashfs.sha256)" \
  /dev/nvme0n1 /etc/janus/release
```

- Les chemins sont ceux de la machine (`/dev/nvme0n1`,
  `/etc/janus/release`), pas ceux de votre poste.
- `-sha256` : `janusctl` ne peut pas lire le bundle sur la machine pour le
  trouver lui-même ; sans lui, ce contrôle est sauté. La signature du
  bundle est vérifiée dans tous les cas. Pour une ISO du site, c'est le
  sha256 de `rootfs.squashfs` listé avec les fichiers de l'image.
- `-network-config` : une [configuration réseau](network-configuration.md)
  appliquée dès le premier démarrage (DHCP sinon).
- La sortie se termine par `[done 100%]`.

### 4. Démarrer le disque installé

Éteindre, **retirer la clé USB** (ou détacher l'ISO), démarrer sur le
disque installé. Au premier démarrage il crée sa propre PKI - affichée
une fois sur la console, à conserver - et s'annonce au Controller.

### 5. Approuver

Le node apparaît dans **Pending approvals** du Controller : **Approve**.
Il ne s'annonce qu'une fois (un marqueur sur STATE le retient), même
après un redémarrage.

Variante : installer sans `-controller-*`, et attacher au premier
démarrage du disque installé un volume `cidata` (méthode 3).

## Matériel physique

Les images démarrent quel que soit le nom du disque : leurs UKI désignent
les partitions par label GPT (`PARTLABEL=BOOT-A-DATA`...), et le noyau les
attend (`dm-mod.waitfor`) - virtio (`/dev/vda`), SATA/SAS/USB
(`/dev/sda`), NVMe (`/dev/nvme0n1`).

- **Disques** : AHCI (SATA), NVMe, USB, virtio-blk et virtio-scsi
  (contrôleur par défaut de Proxmox), VMware pvscsi, contrôleurs RAID/HBA
  Broadcom MegaRAID (Dell PERC), Broadcom/LSI SAS (mpt3sas), Microchip
  SmartPQI (HPE).
- **Cartes réseau** : Intel e1000, e1000e, igb, igc, ixgbe, i40e ;
  Realtek r8169 ; Broadcom tg3, bnxt ; Mellanox ConnectX-4 et suivantes
  (mlx5) ; VMware vmxnet3 ; virtio. Aucun fichier de firmware n'est
  embarqué : une carte qui en exige un (Intel ice, Broadcom bnx2x...)
  n'est pas prise en charge.
- **Processeurs** : tous les cœurs (SMP, jusqu'à 512), x2APIC, NUMA.
- **Console** : l'écran (framebuffer UEFI) et le port série `ttyS0` -
  identifiants du premier démarrage compris.
- Une seule installation de Janus par machine : deux disques Janus
  porteraient les mêmes labels.

## Récupérer les identifiants PKI d'un node (toutes les méthodes)

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
