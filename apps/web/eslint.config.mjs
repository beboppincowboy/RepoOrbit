import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import tseslint from "typescript-eslint";

// Import boundaries. Each regex covers the "@/" alias and relative paths that climb out of the importing folder.
// Flat config replaces, rather than merges, a rule's options when several entries match one file, so the file
// sets below must not overlap and each lists everything it forbids.
const features = (...names) => ({
  regex: `(@/features/|(\\.\\./)+(features/)?)(${names.join("|")})(/|$)`,
  message: `This folder must not import these features: ${names.join(", ")}.`,
});
const unmigrated = (...folders) => ({
  regex: `(@/|(\\.\\./)+)(${folders.join("|")})(/|$)`,
  message: `Shared and feature code must not import these folders: ${folders.join(", ")}.`,
});
const boundary = (files, patterns, ignores = []) => ({
  files,
  ignores,
  rules: { "no-restricted-imports": ["error", { patterns }] },
});
const legacy = unmigrated("lib", "components", "routes");

export default tseslint.config(
  { ignores: [".output/**", "dist/**", "node_modules/**", "src/routeTree.gen.ts", "test-results/**", "playwright-report/**"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["src/**/*.{ts,tsx}"],
    plugins: { "react-hooks": reactHooks },
    languageOptions: { globals: globals.browser },
    rules: { ...reactHooks.configs.recommended.rules },
  },
  { files: ["tests/**/*.ts", "*.ts", "*.mjs"], languageOptions: { globals: globals.node } },
  boundary(["src/shared/**/*.{ts,tsx}"], [
    { regex: "(@/|(\\.\\./)+)features(/|$)", message: "Shared code must not import features." },
    legacy,
  ]),
  boundary(["src/features/issues/**/*.{ts,tsx}"], [features("auth", "repositories", "board", "analytics"), legacy]),
  boundary(["src/features/auth/**/*.{ts,tsx}"], [features("repositories", "board", "analytics"), legacy], ["src/features/auth/api.ts"]),
  // Transitional: signing out clears saved issue drafts, which still live in lib/drafts.
  boundary(["src/features/auth/api.ts"], [
    features("repositories", "board", "analytics"),
    unmigrated("components", "routes"),
    {
      regex: "(@/|(\\.\\./)+)lib(/(?!drafts$)|$)",
      message: "The only module this file may import from lib is lib/drafts.",
    },
  ]),
  boundary(["src/features/repositories/**/*.{ts,tsx}"], [features("board", "analytics"), legacy]),
  boundary(["src/features/board/**/*.{ts,tsx}"], [features("auth", "analytics"), legacy]),
  boundary(["src/features/analytics/**/*.{ts,tsx}"], [features("auth", "board"), legacy]),
  // lib/drafts is reachable from the auth feature, so it must not lead back into any feature.
  boundary(["src/lib/drafts.ts"], [
    { regex: "(@/|(\\.\\./)*)features(/|$)", message: "lib/drafts must not import features." },
    unmigrated("components", "routes"),
  ]),
);
