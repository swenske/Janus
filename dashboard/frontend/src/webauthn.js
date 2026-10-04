// The browser side of passkeys: the Controller's options (JSON, binary as
// base64url) to navigator.credentials, and the credential back to JSON.

function toBuf(s) {
  let b = s.replace(/-/g, '+').replace(/_/g, '/')
  while (b.length % 4) b += '='
  return Uint8Array.from(atob(b), (c) => c.charCodeAt(0)).buffer
}

function fromBuf(buf) {
  let s = ''
  for (const b of new Uint8Array(buf)) s += String.fromCharCode(b)
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

export function passkeysSupported() {
  return typeof window !== 'undefined' && !!window.PublicKeyCredential && !!navigator.credentials
}

// createPasskey makes a passkey for the Controller's creation options.
export async function createPasskey(options) {
  const pk = options.publicKey
  const cred = await navigator.credentials.create({
    publicKey: {
      ...pk,
      challenge: toBuf(pk.challenge),
      user: { ...pk.user, id: toBuf(pk.user.id) },
      excludeCredentials: (pk.excludeCredentials || []).map((c) => ({ ...c, id: toBuf(c.id) })),
    },
  })
  return {
    id: cred.id,
    rawId: fromBuf(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment || undefined,
    response: {
      attestationObject: fromBuf(cred.response.attestationObject),
      clientDataJSON: fromBuf(cred.response.clientDataJSON),
      transports: cred.response.getTransports ? cred.response.getTransports() : [],
    },
    clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
  }
}

// signWithPasskey signs the Controller's challenge with one of the account's
// passkeys.
export async function signWithPasskey(options) {
  const pk = options.publicKey
  const cred = await navigator.credentials.get({
    publicKey: {
      ...pk,
      challenge: toBuf(pk.challenge),
      allowCredentials: (pk.allowCredentials || []).map((c) => ({ ...c, id: toBuf(c.id) })),
    },
  })
  return {
    id: cred.id,
    rawId: fromBuf(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment || undefined,
    response: {
      authenticatorData: fromBuf(cred.response.authenticatorData),
      clientDataJSON: fromBuf(cred.response.clientDataJSON),
      signature: fromBuf(cred.response.signature),
      userHandle: cred.response.userHandle ? fromBuf(cred.response.userHandle) : undefined,
    },
    clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
  }
}
