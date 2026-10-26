import{C as e,c as t,f as n,h as r,l as i,p as a}from"./api-DrhdxpWB.js";import{Ft as o,Ot as s,y as c}from"./index-DXllKcxV.js";import{t as l}from"./ModuleConfigEditor-C_CW34EQ.js";import{t as u}from"./Module-5d1A6_bD.js";var d=e(),f=`https://janus.sw-servers.net/docs/vrrp.md`,p=`# keepalived.conf - see docs/vrrp.md.
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
`,m={MASTER:`ok`,BACKUP:`info`,FAULT:`danger`,INIT:`warn`,STOP:``};function h(){let e=c(`/api/network/vrrp`,{every:3e3}),h=e.data;return h?.state===`not_enabled`?(0,d.jsx)(u,{module:`vrrp`}):(0,d.jsxs)(d.Fragment,{children:[(0,d.jsx)(r,{title:`VRRP · keepalived`,subtitle:`Virtual IPs shared by several nodes, moved to another one when this one fails - or when its HAProxy stops answering`,actions:(0,d.jsxs)(`a`,{className:`button small`,href:f,target:`_blank`,rel:`noreferrer`,children:[`VRRP guide `,(0,d.jsx)(s,{size:12})]})}),e.error&&(0,d.jsx)(n,{error:e.error}),(0,d.jsxs)(i,{title:`Instances`,icon:o,actions:h&&(0,d.jsxs)(t,{tone:h.haproxy_healthy?`ok`:`danger`,dot:!0,children:[`HAProxy `,h.haproxy_healthy?`answers`:`not answering`]}),children:[h?h.configured?h.error?(0,d.jsx)(`div`,{className:`error-box`,children:h.error}):h.instances.length?(0,d.jsx)(`div`,{className:`table-wrap`,children:(0,d.jsxs)(`table`,{children:[(0,d.jsx)(`thead`,{children:(0,d.jsxs)(`tr`,{children:[(0,d.jsx)(`th`,{children:`Instance`}),(0,d.jsx)(`th`,{children:`State`}),(0,d.jsx)(`th`,{children:`Interface`}),(0,d.jsx)(`th`,{children:`VRID`}),(0,d.jsx)(`th`,{children:`Priority`}),(0,d.jsx)(`th`,{children:`Virtual IPs`}),(0,d.jsx)(`th`,{children:`Since`})]})}),(0,d.jsx)(`tbody`,{children:h.instances.map(e=>(0,d.jsxs)(`tr`,{children:[(0,d.jsx)(`td`,{className:`mono`,children:e.name}),(0,d.jsx)(`td`,{children:(0,d.jsx)(t,{tone:m[e.state],dot:!0,children:e.state})}),(0,d.jsx)(`td`,{className:`mono`,children:e.interface}),(0,d.jsx)(`td`,{className:`mono`,children:e.vrid}),(0,d.jsxs)(`td`,{className:`mono`,title:`effective / configured`,children:[e.effective_priority,`/`,e.priority]}),(0,d.jsx)(`td`,{className:`mono`,children:(e.virtual_ips||[]).join(` `)}),(0,d.jsx)(`td`,{className:`small`,children:e.last_transition_unix?new Date(e.last_transition_unix*1e3).toLocaleString():`–`})]},e.name))})]})}):(0,d.jsx)(`span`,{className:`muted small`,children:`keepalived is starting…`}):(0,d.jsx)(`span`,{className:`muted small`,children:`No keepalived.conf is saved: keepalived doesn't run. Write one below.`}):(0,d.jsx)(a,{}),h&&(0,d.jsxs)(`p`,{className:`small muted`,style:{marginBottom:0},children:[`Track `,(0,d.jsx)(`span`,{className:`mono`,children:h.haproxy_health_file}),` (track_file, weight 0) to give the virtual IPs up when this node's HAProxy stops answering. With the firewall extension, accept VRRP: `,(0,d.jsx)(`span`,{className:`mono`,children:`ip protocol 112 accept`}),`.`]})]}),(0,d.jsx)(`div`,{style:{marginTop:`1rem`},children:(0,d.jsx)(l,{base:`/api/network/vrrp`,applyMethod:`NetworkService/VRRPApplyConfig`,readMethod:`NetworkService/VRRPGetConfig`,file:`keepalived.conf`,daemon:`keepalived`,starter:p,applyNote:`keepalived checks it, then reloads it: instances whose state doesn't change keep their virtual IPs.`,onApplied:e.reload})})]})}export{h as default};