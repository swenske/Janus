package main

// What each command does to a node - what the Claude Code skill
// (claude-code/janusctl, docs/guide/claude-code.md) lets an assistant
// run when it helps, run when the user asked for that change, or run
// only when told to do exactly that. Every command of the tree has an
// entry (TestEveryCommandHasAnEffect): a new command says here what it
// does.

// effect is a command's class.
type effect int

const (
	effectUnset effect = iota
	// effectReads: nothing changes, on the node or here.
	effectReads
	// effectChanges: changes the node, or files on this machine.
	effectChanges
	// effectCritical: reboots, interrupts or moves traffic, wipes, or
	// touches the node's trust or credentials.
	effectCritical
)

func (e effect) String() string {
	return [...]string{"?", "reads", "changes", "critical"}[e]
}

// effectOf is a command's class and, when one helps, what to know
// before running it.
type effectOf struct {
	effect
	note string
}

// effects is keyed by the command's path below janusctl.
var effects = map[string]effectOf{
	"login":   {effectChanges, "interactive (browser, device code, SSH key passphrase): the user signs in, not the assistant"},
	"context": {effectChanges, "list only reads"},
	"nodes":   {effectReads, ""},
	"version": {effectReads, ""},
	"tui":     {effectReads, "interactive: only -once prints a frame and exits"},

	"system info":                  {effectReads, ""},
	"system hostname":              {effectReads, ""},
	"system services":              {effectReads, ""},
	"system service":               {effectCritical, "stopping or restarting a service interrupts what it does"},
	"system logs":                  {effectReads, "-f follows forever: use -n LINES"},
	"system events":                {effectReads, "streams forever: run it under timeout"},
	"system audit":                 {effectReads, "streams forever: run it under timeout"},
	"system dmesg":                 {effectReads, "-f follows forever"},
	"system boots":                 {effectReads, ""},
	"system log-forwarding status": {effectReads, ""},
	"system log-forwarding get":    {effectReads, ""},
	"system log-forwarding set":    {effectChanges, ""},
	"system log-forwarding test":   {effectChanges, "sends a record to the target"},
	"system stats":                 {effectReads, ""},
	"system systemstat":            {effectReads, ""},
	"system ps":                    {effectReads, ""},
	"system netdev":                {effectReads, ""},
	"system netstat":               {effectReads, ""},
	"system clients":               {effectReads, ""},
	"system mounts":                {effectReads, ""},
	"system du":                    {effectReads, ""},
	"system ls":                    {effectReads, ""},
	"system cat":                   {effectReads, ""},
	"system cp":                    {effectReads, "a tar: give -o FILE"},
	"system support":               {effectReads, ""},
	"system pcap":                  {effectReads, "packets may hold clients' data: -duration and -o FILE, never stdout"},
	"system probe":                 {effectReads, "sends traffic from the node"},
	"system metrics":               {effectChanges, "shows the settings without flags"},
	"system console-budget":        {effectChanges, "shows the budget without flags"},
	"system node-exporter":         {effectChanges, "shows the settings without flags"},
	"system state export":          {effectCritical, "the file holds the node's private keys"},
	"system state import":          {effectCritical, "the node takes another's state and restarts into it"},
	"system sysctl list":           {effectReads, ""},
	"system sysctl get":            {effectReads, ""},
	"system sysctl set":            {effectChanges, "on trial: reverts by itself unless confirmed"},
	"system sysctl reset":          {effectChanges, "on trial: reverts by itself unless confirmed"},
	"system sysctl confirm":        {effectChanges, ""},
	"system sysctl cancel":         {effectChanges, ""},
	"system sysctl history":        {effectReads, ""},
	"system sysctl observed":       {effectReads, ""},
	"system reboot":                {effectCritical, ""},
	"system shutdown":              {effectCritical, "nothing powers it back on remotely"},
	"system restart":               {effectChanges, "janusd only: HAProxy keeps serving"},
	"system reset":                 {effectCritical, "wipes the node's state: a new CA, configuration gone"},

	"haproxy show-info":      {effectReads, ""},
	"haproxy stats":          {effectReads, ""},
	"haproxy backends":       {effectReads, ""},
	"haproxy server-state":   {effectChanges, "drain and maint take the server out of the traffic; HAProxy's next reload (apply-config, file-put -reload...) takes haproxy.cfg's state again"},
	"haproxy reload":         {effectChanges, "seamless, but what server-state, map-set and acl-add changed is gone: a drained server takes traffic again - check haproxy backends first"},
	"haproxy get-config":     {effectReads, ""},
	"haproxy apply-config":   {effectChanges, "validated on the node first, reloaded without dropping connections"},
	"haproxy config-history": {effectReads, ""},
	"haproxy config-revert":  {effectChanges, ""},
	"haproxy map-list":       {effectReads, ""},
	"haproxy map-get":        {effectReads, ""},
	"haproxy map-set":        {effectChanges, "in memory only: the next reload reads the map's file again"},
	"haproxy map-delete":     {effectChanges, "in memory only: the next reload reads the map's file again"},
	"haproxy map-lookup":     {effectReads, ""},
	"haproxy acl-add":        {effectChanges, "in memory only: the next reload reads the list's file again"},
	"haproxy acl-delete":     {effectChanges, "in memory only: the next reload reads the list's file again"},
	"haproxy acl-lookup":     {effectReads, ""},
	"haproxy tables":         {effectReads, ""},
	"haproxy table":          {effectReads, ""},
	"haproxy table-clear":    {effectChanges, ""},
	"haproxy sessions":       {effectReads, ""},
	"haproxy session-kill":   {effectChanges, ""},
	"haproxy errors":         {effectReads, ""},
	"haproxy cert-list":      {effectReads, ""},
	"haproxy cert-upload":    {effectChanges, "pass the PEM by path, never print it"},
	"haproxy cert-delete":    {effectChanges, ""},
	"haproxy files":          {effectReads, ""},
	"haproxy file-get":       {effectReads, ""},
	"haproxy file-put":       {effectChanges, ""},
	"haproxy file-delete":    {effectChanges, ""},
	"haproxy acme status":    {effectReads, ""},
	"haproxy acme get":       {effectReads, ""},
	"haproxy acme check":     {effectReads, ""},
	"haproxy acme apply":     {effectChanges, ""},
	"haproxy acme renew":     {effectChanges, "orders certificates: the CA's rate limits count"},
	"haproxy acme path":      {effectChanges, "shows the path without arguments"},
	"haproxy acme test-path": {effectReads, ""},
	"haproxy acme csr":       {effectChanges, ""},
	"haproxy acme install":   {effectChanges, ""},

	"network status":           {effectReads, ""},
	"network neighbors":        {effectReads, ""},
	"network get":              {effectReads, ""},
	"network apply":            {effectChanges, "on trial: janusctl confirms over a new connection, else the node reverts by itself"},
	"network confirm":          {effectChanges, ""},
	"network modules":          {effectReads, ""},
	"network firewall status":  {effectReads, ""},
	"network firewall get":     {effectReads, ""},
	"network firewall check":   {effectReads, ""},
	"network firewall apply":   {effectChanges, "on trial: janusctl confirms over a new connection, else the node reverts by itself"},
	"network firewall confirm": {effectChanges, ""},
	"network firewall sets":    {effectReads, ""},
	"network firewall set-add": {effectChanges, ""},
	"network firewall set-del": {effectChanges, ""},
	"network vrrp status":      {effectReads, ""},
	"network vrrp get":         {effectReads, ""},
	"network vrrp check":       {effectReads, ""},
	"network vrrp apply":       {effectChanges, "no trial: a priority change can move the virtual IPs at once"},
	"network bgp status":       {effectReads, ""},
	"network bgp get":          {effectReads, ""},
	"network bgp check":        {effectReads, ""},
	"network bgp apply":        {effectChanges, "no trial: routes change at once"},
	"network consul status":    {effectReads, ""},
	"network consul get":       {effectReads, ""},
	"network consul check":     {effectReads, ""},
	"network consul apply":     {effectChanges, "the agent restarts"},

	"access trust":       {effectReads, ""},
	"access trust-set":   {effectCritical, "pins a fleet's root on the node"},
	"access trust-reset": {effectCritical, "the fleet - a Controller too - loses the node"},
	"access rotate-ca":   {effectCritical, ""},

	"cluster list":           {effectReads, ""},
	"cluster create":         {effectCritical, ""},
	"cluster checkout":       {effectChanges, "writes files on this machine"},
	"cluster apply":          {effectChanges, "member by member, waiting for the stick tables"},
	"cluster check":          {effectChanges, "renews certificates and rotates the ticket ring when due"},
	"cluster add":            {effectChanges, ""},
	"cluster remove":         {effectCritical, "the member is drained, then leaves"},
	"cluster replace":        {effectCritical, "the traffic moves from OLD to NEW"},
	"cluster delete":         {effectCritical, ""},
	"cluster vars":           {effectChanges, "shows them without KEY=VALUE"},
	"cluster certificate":    {effectChanges, "pass the PEM by path, never print it"},
	"cluster tickets":        {effectChanges, ""},
	"cluster balancer":       {effectChanges, "shows the hook without flags"},
	"cluster journal":        {effectReads, ""},
	"cluster get":            {effectReads, ""},
	"cluster status":         {effectReads, ""},
	"cluster csr":            {effectChanges, ""},
	"cluster set":            {effectCritical, "HAProxy restarts with the peers"},
	"cluster leave":          {effectCritical, ""},
	"cluster release":        {effectChanges, "the cluster then reports the node as drifted"},
	"cluster drain":          {effectCritical, "the virtual IPs and routes move to the peers"},
	"cluster undrain":        {effectCritical, "the node takes traffic again"},
	"cluster pull":           {effectChanges, ""},
	"cluster rotate-tickets": {effectChanges, ""},

	"pki generate-client-config": {effectCritical, "issues a credential"},

	"fleet init":           {effectCritical, "a new fleet: its recovery kit goes offline, kept by the user"},
	"fleet recover":        {effectCritical, ""},
	"fleet adopt":          {effectCritical, "pins the fleet's root on the node"},
	"fleet sync":           {effectChanges, ""},
	"fleet status":         {effectReads, ""},
	"fleet export":         {effectChanges, "writes files on this machine"},
	"fleet forget":         {effectChanges, ""},
	"fleet issuer list":    {effectReads, ""},
	"fleet issuer request": {effectChanges, "writes a request and a key on this machine"},
	"fleet issuer sign":    {effectCritical, "a new issuing CA the fleet trusts"},
	"fleet issuer accept":  {effectCritical, ""},
	"fleet issuer revoke":  {effectCritical, ""},

	"lifecycle install":        {effectCritical, "partitions and writes a disk"},
	"lifecycle upgrade":        {effectCritical, "reboots into the new release"},
	"lifecycle rollback":       {effectCritical, "reboots into the other slot"},
	"lifecycle upload-release": {effectChanges, "stages a bundle: nothing runs it until upgrade"},

	"image seed-controller": {effectChanges, "writes onto a disk image on this machine"},
	"image seed-fleet":      {effectChanges, "writes onto a disk image on this machine"},
	"image seed-network":    {effectChanges, "writes onto a disk image on this machine"},

	"completion": {effectReads, ""},
	"help":       {effectReads, ""},
}
