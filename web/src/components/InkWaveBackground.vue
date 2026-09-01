<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'

// Ink-wave background — the login gate's living stage.
//
// One full-screen WebGL shader draws slow monochrome wave bands (layered
// sines warped by fbm noise); the cursor is a smooth-lerped uniform that
// lifts the local wave amplitude and carries a soft light bloom, so the
// ink feels attracted to the pointer. Everything stays grayscale and low
// contrast — the premium feel comes from restraint, not color.
//
// Degradation ladder (each step is a complete look, never a blank):
//   WebGL missing        → canvas hidden; the CSS radial stage + grain remain
//   prefers-reduced-motion → one rendered frame, no rAF loop
//   tab hidden           → rAF paused (visibilitychange)
//
// Tone is read from the theme at mount and on html.dark flips: dark shows
// pale ink bands on graphite, light shows graphite bands on paper. The
// `ink` prop pins the always-dark native front-door tone instead.
const props = defineProps<{ ink?: boolean }>()

const host = ref<HTMLDivElement | null>(null)

onMounted(() => {
  const el = host.value
  if (!el) return
  const canvas = document.createElement('canvas')
  canvas.setAttribute('aria-hidden', 'true')
  canvas.style.cssText = 'position:absolute;inset:0;width:100%;height:100%;display:block'
  el.appendChild(canvas)
  const glCtx = canvas.getContext('webgl', { antialias: false, alpha: true, powerPreference: 'low-power' })
  if (!glCtx) {
    canvas.remove() // CSS stage below stays the whole look
    return
  }
  const gl = glCtx

  const vsrc = `
attribute vec2 p;
void main() { gl_Position = vec4(p, 0.0, 1.0); }`
  // Fragment: three wave bands of drifting frequency/phase, fbm-warped so
  // the lines breathe instead of ticking; u_mouse adds local amplitude and
  // a faint bloom. Output is a single-channel veil blended over whatever
  // CSS paints underneath (the canvas is alpha).
  const fsrc = `
precision mediump float;
uniform vec2 u_res;
uniform vec2 u_mouse;
uniform float u_time;
uniform float u_light;   // 1 = pale bands on dark, 0 = dark bands on light
uniform float u_strength;
float hash(vec2 p) {
  return fract(sin(dot(p, vec2(127.1, 311.7))) * 43758.5453123);
}
float noise(vec2 p) {
  vec2 i = floor(p);
  vec2 f = fract(p);
  vec2 u = f * f * (3.0 - 2.0 * f);
  return mix(mix(hash(i), hash(i + vec2(1.0, 0.0)), u.x),
             mix(hash(i + vec2(0.0, 1.0)), hash(i + vec2(1.0, 1.0)), u.x), u.y);
}
float fbm(vec2 p) {
  float v = 0.0;
  float a = 0.5;
  for (int i = 0; i < 4; i++) {
    v += a * noise(p);
    p *= 2.03;
    a *= 0.5;
  }
  return v;
}
void main() {
  vec2 uv = gl_FragCoord.xy / u_res;
  vec2 st = uv * vec2(u_res.x / u_res.y, 1.0);

  // Smoothed cursor in the same aspect space, with a gentle pull toward
  // the band field so the ink leans after the pointer.
  vec2 m = u_mouse * vec2(u_res.x / u_res.y, 1.0);
  float md = length(st - m);
  float pull = exp(-md * 3.2);

  float warp = fbm(st * 1.7 + vec2(u_time * 0.020, u_time * -0.014));
  float field = 0.0;
  field += sin((st.x * 2.1 + warp * 2.6) + u_time * 0.24 + sin(st.y * 2.4 + u_time * 0.11) * 1.4);
  field += sin((st.x * 3.4 - warp * 1.9) - u_time * 0.17 + sin(st.y * 3.1 - u_time * 0.07) * 1.1) * 0.62;
  field += sin((st.x * 1.3 + warp * 3.4) + u_time * 0.09) * 0.45;
  // Cursor gain: waves stand up and sharpen around the pointer.
  field *= 1.0 + pull * 0.9;
  float band = smoothstep(0.62, 0.98, abs(field));

  float shade = band * (0.16 + pull * 0.10) * u_strength;
  // Cursor bloom: a faint wide halo riding the pointer.
  float bloom = exp(-md * 5.0) * 0.05 * u_strength;

  float a = clamp(shade + bloom, 0.0, 0.30);
  float g = u_light;
  gl_FragColor = vec4(vec3(g), a);
}`

  function sh(type: number, src: string): WebGLShader | null {
    const s = gl.createShader(type)
    if (!s) return null
    gl.shaderSource(s, src)
    gl.compileShader(s)
    if (!gl.getShaderParameter(s, gl.COMPILE_STATUS)) {
      gl.deleteShader(s)
      return null
    }
    return s
  }
  const vs = sh(gl.VERTEX_SHADER, vsrc)
  const fs = sh(gl.FRAGMENT_SHADER, fsrc)
  const prog = gl.createProgram()
  if (!vs || !fs || !prog) {
    canvas.remove()
    return
  }
  gl.attachShader(prog, vs)
  gl.attachShader(prog, fs)
  gl.linkProgram(prog)
  if (!gl.getProgramParameter(prog, gl.LINK_STATUS)) {
    canvas.remove()
    return
  }
  gl.useProgram(prog)
  const buf = gl.createBuffer()
  gl.bindBuffer(gl.ARRAY_BUFFER, buf)
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW)
  const loc = gl.getAttribLocation(prog, 'p')
  gl.enableVertexAttribArray(loc)
  gl.vertexAttribPointer(loc, 2, gl.FLOAT, false, 0, 0)
  gl.enable(gl.BLEND)
  gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA)

  const uRes = gl.getUniformLocation(prog, 'u_res')
  const uMouse = gl.getUniformLocation(prog, 'u_mouse')
  const uTime = gl.getUniformLocation(prog, 'u_time')
  const uLight = gl.getUniformLocation(prog, 'u_light')
  const uStrength = gl.getUniformLocation(prog, 'u_strength')

  // Tone: pinned for the native front door, otherwise read from the theme.
  let light = 1
  let strength = 1
  function readTone() {
    if (props.ink) {
      light = 1
      strength = 1
      return
    }
    light = document.documentElement.classList.contains('dark') ? 1 : 0
    // Pale bands need slightly less veil on paper than on graphite.
    strength = light ? 1 : 0.8
  }
  readTone()

  const dpr = Math.min(window.devicePixelRatio || 1, 2)
  const stage: HTMLDivElement = el
  function resize() {
    const w = Math.max(1, Math.floor(stage.clientWidth * dpr))
    const h = Math.max(1, Math.floor(stage.clientHeight * dpr))
    if (canvas.width !== w || canvas.height !== h) {
      canvas.width = w
      canvas.height = h
      gl.viewport(0, 0, w, h)
    }
  }

  // The pointer is chased with easing (lerp), never snapped — the ink has
  // inertia. A still cursor keeps the last position as a resting bloom.
  const target = { x: 0.5, y: 0.55 }
  const smooth = { x: 0.5, y: 0.55 }
  function onPointer(e: PointerEvent) {
    target.x = e.clientX / window.innerWidth
    target.y = 1 - e.clientY / window.innerHeight
  }
  window.addEventListener('pointermove', onPointer, { passive: true })

  const t0 = performance.now()
  let raf = 0
  const reduced = window.matchMedia('(prefers-reduced-motion: reduce)')
  function frame(now: number) {
    raf = requestAnimationFrame(frame)
    resize()
    smooth.x += (target.x - smooth.x) * 0.045
    smooth.y += (target.y - smooth.y) * 0.045
    gl.uniform2f(uRes, canvas.width, canvas.height)
    gl.uniform2f(uMouse, smooth.x, smooth.y)
    gl.uniform1f(uTime, (now - t0) / 1000)
    gl.uniform1f(uLight, light)
    gl.uniform1f(uStrength, strength)
    gl.drawArrays(gl.TRIANGLES, 0, 3)
    if (reduced.matches) {
      cancelAnimationFrame(raf) // one still frame is the whole degrade
      raf = 0
    }
  }
  function onVisibility() {
    if (document.hidden) {
      cancelAnimationFrame(raf)
      raf = 0
    } else if (!reduced.matches && !raf) {
      raf = requestAnimationFrame(frame)
    }
  }
  function onMotionChange() {
    if (reduced.matches) {
      cancelAnimationFrame(raf)
      raf = 0
    } else if (!raf) {
      raf = requestAnimationFrame(frame)
    }
  }
  function onThemeFlip() {
    readTone()
  }
  reduced.addEventListener('change', onMotionChange)
  document.addEventListener('visibilitychange', onVisibility)
  const themeObserver = new MutationObserver(onThemeFlip)
  themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })

  resize()
  raf = requestAnimationFrame(frame)

  onBeforeUnmount(() => {
    cancelAnimationFrame(raf)
    window.removeEventListener('pointermove', onPointer)
    document.removeEventListener('visibilitychange', onVisibility)
    reduced.removeEventListener('change', onMotionChange)
    themeObserver.disconnect()
  })
})
</script>

<template>
  <div ref="host" class="ink-wave" aria-hidden="true"></div>
</template>

<style scoped>
.ink-wave {
  position: absolute;
  inset: 0;
  overflow: hidden;
  pointer-events: none;
}
</style>
