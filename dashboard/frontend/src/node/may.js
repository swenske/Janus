import { createContext, useContext } from 'react'
import { lockedDomain } from './cluster.js'

// MayContext answers whether the page's account may call an RPC on the
// node ("Service/Method", from /api/me's may - internal/rbac): what it
// may not do isn't offered. The node enforces it anyway. may(method,
// 'validate') asks for an apply RPC's validate-only call - the editors'
// Validate/Check button, kept while the page is read-only.
export const MayContext = createContext(() => true)

export function useMay() {
  return useContext(MayContext)
}

// mayFor builds the page's may: the account's RPCs (/api/me's may), not a
// write to what the node's cluster manages here (the node refuses it -
// ClusterLockNotice says so on those pages), and, while the page is
// read-only, only those that read (/api/me's reads, internal/rbac
// Effect) - or validate.
export function mayFor({ may, reads, cluster, readOnly }) {
  return (method, purpose) => {
    if (!may || !may.includes(method) || lockedDomain(cluster, method)) return false
    if (!readOnly || purpose === 'validate') return true
    return !!reads && reads.includes(method)
  }
}

// changesSomething reports whether the account may change anything on
// the node from this page - else the Read-only switch has nothing to
// hide and isn't shown. A Controller that doesn't say what reads (no
// reads): no switch.
export function changesSomething({ may, reads, cluster }) {
  if (!may || !reads) return false
  return may.some((m) => !reads.includes(m) && !lockedDomain(cluster, m))
}
