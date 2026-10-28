import{C as e,c as t,f as n,h as r,l as i,p as a}from"./api-DGbpmHlm.js";import{C as o,Mt as s,rt as c}from"./index-DT7MS4f7.js";import{t as l}from"./ModuleConfigEditor-CG1gxZTN.js";import{t as u}from"./Module-DwX7fYPI.js";var d=e(),f=`https://janus.sw-servers.net/docs/bgp.md`,p=`# bird.conf - see docs/bgp.md.
# Protocols named haproxy_* are kept down while this node's HAProxy
# doesn't answer: put the routes HAProxy serves (an anycast address) there.

router id 192.0.2.1;            # this node's own address

protocol device {}

# The anycast address HAProxy serves - also an address of this node
# (network configuration) - withdrawn while HAProxy doesn't answer.
protocol static haproxy_anycast {
    ipv4;
    route 192.0.2.100/32 blackhole;
}

protocol bgp upstream {
    local 192.0.2.1 as 65001;
    neighbor 192.0.2.254 as 65000;
    # password "secret";        # TCP MD5, if the peer asks for one
    ipv4 {
        import none;
        export where proto = "haproxy_anycast";
    };
}
`,m={up:`ok`,start:`warn`,down:``,stop:`warn`};function h(e){return e.channels.length?e.channels.map(e=>`${e.name} ${e.imported} in / ${e.exported} out`).join(`, `):`–`}function g(){let e=o(`/api/network/bgp`,{every:3e3}),g=e.data;if(g?.state===`not_enabled`)return(0,d.jsx)(u,{module:`bgp`});let _=g?.protocols.filter(e=>e.held_down)||[];return(0,d.jsxs)(d.Fragment,{children:[(0,d.jsx)(r,{title:`BGP · bird`,subtitle:`Announce this node's addresses to your routers - an anycast address withdrawn while HAProxy doesn't answer`,actions:(0,d.jsxs)(`a`,{className:`button small`,href:f,target:`_blank`,rel:`noreferrer`,children:[`BGP guide `,(0,d.jsx)(s,{size:12})]})}),e.error&&(0,d.jsx)(n,{error:e.error}),(0,d.jsxs)(i,{title:`Protocols`,icon:c,actions:g&&(0,d.jsxs)(`div`,{className:`row`,children:[g.version&&(0,d.jsxs)(`span`,{className:`small muted`,children:[`BIRD `,g.version,` · router ID `,(0,d.jsx)(`span`,{className:`mono`,children:g.router_id})]}),(0,d.jsxs)(t,{tone:g.haproxy_healthy?`ok`:`danger`,dot:!0,children:[`HAProxy `,g.haproxy_healthy?`answers`:`not answering`]})]}),children:[g?g.configured?g.error?(0,d.jsx)(`div`,{className:`error-box`,children:g.error}):g.protocols.length?(0,d.jsx)(`div`,{className:`table-wrap`,children:(0,d.jsxs)(`table`,{children:[(0,d.jsx)(`thead`,{children:(0,d.jsxs)(`tr`,{children:[(0,d.jsx)(`th`,{children:`Protocol`}),(0,d.jsx)(`th`,{children:`Type`}),(0,d.jsx)(`th`,{children:`State`}),(0,d.jsx)(`th`,{children:`Info`}),(0,d.jsx)(`th`,{children:`Neighbor`}),(0,d.jsx)(`th`,{children:`Routes`}),(0,d.jsx)(`th`,{children:`Since`})]})}),(0,d.jsx)(`tbody`,{children:g.protocols.map(e=>(0,d.jsxs)(`tr`,{children:[(0,d.jsx)(`td`,{className:`mono`,children:e.name}),(0,d.jsx)(`td`,{children:e.protocol}),(0,d.jsx)(`td`,{children:(0,d.jsx)(t,{tone:m[e.state],dot:!0,children:e.state})}),(0,d.jsx)(`td`,{className:`small`,children:e.held_down?(0,d.jsx)(t,{tone:`danger`,children:`held down: HAProxy doesn't answer`}):(0,d.jsxs)(d.Fragment,{children:[e.info,e.last_error&&!e.info.includes(e.last_error)&&(0,d.jsx)(`div`,{className:`muted`,children:e.last_error})]})}),(0,d.jsx)(`td`,{className:`mono small`,children:e.neighbor_address?`${e.neighbor_address} AS${e.neighbor_as}`:`–`}),(0,d.jsx)(`td`,{className:`small`,children:h(e)}),(0,d.jsx)(`td`,{className:`small mono`,children:e.since})]},e.name))})]})}):(0,d.jsx)(`span`,{className:`muted small`,children:`BIRD is starting…`}):(0,d.jsx)(`span`,{className:`muted small`,children:`No bird.conf is saved: BIRD doesn't run. Write one below.`}):(0,d.jsx)(a,{}),g&&(0,d.jsxs)(`p`,{className:`small muted`,style:{marginBottom:0},children:[_.length>0?`${_.map(e=>e.name).join(`, `)} held down until HAProxy answers again. `:``,`Protocols named `,(0,d.jsx)(`span`,{className:`mono`,children:`haproxy_*`}),` are kept down while this node's HAProxy doesn't answer. With the firewall extension, accept BGP:`,` `,(0,d.jsx)(`span`,{className:`mono`,children:`tcp dport 179 accept`}),`.`]})]}),(0,d.jsx)(`div`,{style:{marginTop:`1rem`},children:(0,d.jsx)(l,{base:`/api/network/bgp`,applyMethod:`NetworkService/BGPApplyConfig`,readMethod:`NetworkService/BGPGetConfig`,file:`bird.conf`,daemon:`BIRD`,starter:p,applyNote:`BIRD checks it, then reconfigures: sessions whose configuration doesn't change stay up.`,onApplied:e.reload})})]})}export{g as default};