// Static asset served through the tunnel regression path — exercises the
// non-root branch of the SPA file server (fileExists lookup, mime by
// extension, io.CopyN body copy) rather than the "/" index route.
window.tunnelStaticTest = function () {
  return "tunnel-static-asset";
};
