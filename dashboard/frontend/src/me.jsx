import { createContext, useContext } from 'react'

// Who the page is signed in as, and what that account's role lets it do -
// the Controller checks it on every request anyway: this only hides what
// would be refused.

const RANK = { reader: 1, operator: 2, admin: 3 }

export const ROLES = [
  { id: 'reader', label: 'Reader', about: 'sees everything, changes nothing' },
  { id: 'operator', label: 'Operator', about: 'runs nodes - HAProxy, services, reboots - and powers machines' },
  { id: 'admin', label: 'Admin', about: 'everything: accounts, the fleet, hypervisors, machines, approvals, updates' },
]

export const MeContext = createContext({ name: '', role: 'reader', needs: [] })

export function useMe() {
  return useContext(MeContext)
}

export function atLeast(role, need) {
  return (RANK[role] || 0) >= RANK[need]
}

// useCan answers whether the account may do what needs a role.
export function useCan() {
  const me = useMe()
  return (need) => atLeast(me.role, need)
}
