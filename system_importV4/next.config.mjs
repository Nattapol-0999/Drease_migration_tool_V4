/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  // The dump files a clinic exports can be a few hundred KB to a few MB. Raise the
  // server-action / route body limit so a multi-file upload is not rejected before it
  // even reaches the BFF proxy. The backend itself caps at 64 MB.
  experimental: {
    serverActions: {
      bodySizeLimit: "64mb",
    },
    // ssh2 loads optional native addons (cpu-features, its own sshcrypto build) behind
    // try/catch and falls back to pure JS. Bundling it makes webpack chase those
    // requires and warn about modules that are meant to be absent, so it is left to
    // node's own require instead.
    serverComponentsExternalPackages: ["ssh2"],
  },
};

export default nextConfig;
