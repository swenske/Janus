import { createContext, useContext } from 'react'

// ClusterContext carries the node's cluster membership (/api/cluster,
// cluster.js): cluster is null outside a cluster - or on a node too old to
// say; reload reads it again.
export const ClusterContext = createContext({ cluster: null, reload: () => {} })

export function useCluster() {
  return useContext(ClusterContext)
}
