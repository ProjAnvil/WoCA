import { getNativeBridge } from './bridge'

export const nativeRequestIDHeader = 'X-Woca-Native-Request-ID'

const base64URLAlphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_'

export interface NativeRequestCancellation {
  headers: Record<string, string>
  release: () => void
}

export function nativeRequestCancellation(
  signal?: AbortSignal,
  inputHeaders: Record<string, string> = {},
): NativeRequestCancellation {
  const bridge = getNativeBridge()
  if (!bridge) return { headers: { ...inputHeaders }, release: () => {} }

  const id = randomRequestID()
  const headers = { ...inputHeaders, [nativeRequestIDHeader]: id }
  let cancellationSent = false
  const cancel = () => {
    if (cancellationSent) return
    cancellationSent = true
    try {
      void bridge.CancelRequest(id).catch(() => {})
    } catch {
      /* Wails is already unavailable; local abort still proceeds. */
    }
  }
  if (signal?.aborted) cancel()
  else signal?.addEventListener('abort', cancel, { once: true })

  return {
    headers,
    release: () => signal?.removeEventListener('abort', cancel),
  }
}

function randomRequestID(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32))
  let encoded = ''
  for (let index = 0; index < bytes.length; index += 3) {
    const first = bytes[index]
    const second = bytes[index + 1]
    const third = bytes[index + 2]
    encoded += base64URLAlphabet[first >> 2]
    encoded += base64URLAlphabet[((first & 0x03) << 4) | ((second ?? 0) >> 4)]
    if (second !== undefined) {
      encoded += base64URLAlphabet[((second & 0x0f) << 2) | ((third ?? 0) >> 6)]
    }
    if (third !== undefined) encoded += base64URLAlphabet[third & 0x3f]
  }
  return encoded
}
