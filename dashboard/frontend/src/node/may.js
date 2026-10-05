import { createContext, useContext } from 'react'

// MayContext answers whether the page's account may call an RPC on the
// node ("Service/Method", from /api/me's may - internal/rbac): what it
// may not do isn't offered. The node enforces it anyway.
export const MayContext = createContext(() => true)

export function useMay() {
  return useContext(MayContext)
}
