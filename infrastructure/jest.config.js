module.exports = {
  testEnvironment: "node",
  roots: ["<rootDir>/test"],
  testMatch: ["**/*.test.ts"],
  // `tsc` compiles alongside the sources (cdk.json runs bin/data-releaser.js), so
  // lib/*.js sits next to lib/*.ts. Jest's default resolution order puts "js"
  // first, which would silently test the last build instead of the current
  // source. Resolve .ts first so the tests always reflect the sources.
  moduleFileExtensions: ["ts", "tsx", "js", "mjs", "cjs", "jsx", "json", "node"],
  transform: {
    "^.+\\.tsx?$": "ts-jest"
  }
};
