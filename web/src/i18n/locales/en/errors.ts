// Standard error-code vocabulary (api-standard.md §1). Keys are the wire
// codes; values the English copy. zh-CN mirrors them. A code without an
// entry here falls back to the server's English message at display time.
export default {
  errors: {
    'S-B-0001': 'Sign-in required.',
    'S-B-0002': 'Your credentials were revoked — sign in again.',
    'S-B-0003': 'You do not have permission for this action.',
    'S-E-0002': 'Your credentials were revoked — sign in again.',
    'S-C-0001': 'Invalid console credentials.',
  },
}
