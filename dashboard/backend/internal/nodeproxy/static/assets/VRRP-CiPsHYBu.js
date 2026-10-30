import{C as e,c as t,f as n,h as r,l as i,p as a}from"./api-zuZeK0Tw.js";import{C as o,Mt as s,h as c,zt as l}from"./index-DLns_kgd.js";import{t as u}from"./ModuleConfigEditor-C16ZphWE.js";import{t as d}from"./Module-NDZ2PxgQ.js";var f=e(),p=`https://janus.sw-servers.net/docs/vrrp.md`,m=`# keepalived.conf - see docs/vrrp.md.
# Every node of the group gets the same file, but for its priority.

track_file haproxy {
    # 0 while this node's HAProxy answers, 1 when it doesn't - kept by janusd.
    file /run/janus/keepalived/haproxy-health
}

vrrp_instance VI_1 {
    state BACKUP
    interface eth0
    virtual_router_id 51
    priority 100            # higher wins: give each node its own
    advert_int 1
    track_file {
        haproxy weight 0    # HAProxy not answering: give the virtual IPs up
    }
    virtual_ipaddress {
        192.0.2.100/24
    }
}
`,h={MASTER:`ok`,BACKUP:`info`,FAULT:`danger`,INIT:`warn`,STOP:``};function g(){let e=o(`/api/network/vrrp`,{every:3e3}),g=e.data;return g?.state===`not_enabled`?(0,f.jsx)(d,{module:`vrrp`}):(0,f.jsxs)(f.Fragment,{children:[(0,f.jsx)(r,{title:`VRRP · keepalived`,subtitle:`Virtual IPs shared by several nodes, moved to another one when this one fails - or when its HAProxy stops answering`,actions:(0,f.jsxs)(`a`,{className:`button small`,href:p,target:`_blank`,rel:`noreferrer`,children:[`VRRP guide `,(0,f.jsx)(s,{size:12})]})}),e.error&&(0,f.jsx)(n,{error:e.error}),(0,f.jsxs)(i,{title:`Instances`,icon:l,actions:g&&(0,f.jsxs)(t,{tone:g.haproxy_healthy?`ok`:`danger`,dot:!0,children:[`HAProxy `,g.haproxy_healthy?`answers`:`not answering`]}),children:[g?g.configured?g.error?(0,f.jsx)(`div`,{className:`error-box`,children:g.error}):g.instances.length?(0,f.jsx)(`div`,{className:`table-wrap`,children:(0,f.jsxs)(`table`,{children:[(0,f.jsx)(`thead`,{children:(0,f.jsxs)(`tr`,{children:[(0,f.jsx)(`th`,{children:`Instance`}),(0,f.jsx)(`th`,{children:`State`}),(0,f.jsx)(`th`,{children:`Interface`}),(0,f.jsx)(`th`,{children:`VRID`}),(0,f.jsx)(`th`,{children:`Priority`}),(0,f.jsx)(`th`,{children:`Virtual IPs`}),(0,f.jsx)(`th`,{children:`Since`})]})}),(0,f.jsx)(`tbody`,{children:g.instances.map(e=>(0,f.jsxs)(`tr`,{children:[(0,f.jsx)(`td`,{className:`mono`,children:e.name}),(0,f.jsx)(`td`,{children:(0,f.jsx)(t,{tone:h[e.state],dot:!0,children:e.state})}),(0,f.jsx)(`td`,{className:`mono`,children:e.interface}),(0,f.jsx)(`td`,{className:`mono`,children:e.vrid}),(0,f.jsxs)(`td`,{className:`mono`,title:`effective / configured`,children:[e.effective_priority,`/`,e.priority]}),(0,f.jsx)(`td`,{className:`mono`,children:(e.virtual_ips||[]).join(` `)}),(0,f.jsx)(`td`,{className:`small`,children:e.last_transition_unix?c(e.last_transition_unix*1e3):`–`})]},e.name))})]})}):(0,f.jsx)(`span`,{className:`muted small`,children:`keepalived is starting…`}):(0,f.jsx)(`span`,{className:`muted small`,children:`No keepalived.conf is saved: keepalived doesn't run. Write one below.`}):(0,f.jsx)(a,{}),g&&(0,f.jsxs)(`p`,{className:`small muted`,style:{marginBottom:0},children:[`Track `,(0,f.jsx)(`span`,{className:`mono`,children:g.haproxy_health_file}),` (track_file, weight 0) to give the virtual IPs up when this node's HAProxy stops answering. With the firewall extension, accept VRRP: `,(0,f.jsx)(`span`,{className:`mono`,children:`ip protocol 112 accept`}),`.`]})]}),(0,f.jsx)(`div`,{style:{marginTop:`1rem`},children:(0,f.jsx)(u,{base:`/api/network/vrrp`,applyMethod:`NetworkService/VRRPApplyConfig`,readMethod:`NetworkService/VRRPGetConfig`,file:`keepalived.conf`,daemon:`keepalived`,starter:m,applyNote:`keepalived checks it, then reloads it: instances whose state doesn't change keep their virtual IPs.`,onApplied:e.reload})})]})}export{g as default};