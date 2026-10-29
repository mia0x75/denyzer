/** @type {import("next").NextConfig} */
const config = {
  output: "export",
  basePath: "/dashboard",
  images: { unoptimized: true },
  trailingSlash: true,
  reactCompiler: true,
};

export default config;
