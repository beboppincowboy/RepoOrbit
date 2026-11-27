import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import tseslint from "typescript-eslint";

// Import boundaries, enforced on the "@/" alias only: boundary() also bans parent-relative and bare "src/" imports so
// they cannot bypass the alias patterns, and dynamic imports are banned so every import is checked.
// Flat config replaces, rather than merges, a rule's options when several entries match one file, so the file
// sets below must not overlap and each lists everything it forbids.
const features = (...names) => ({
  regex: `^@/features/(${names.join("|")})(/|$)`,
  message: `This folder must not import these features: ${names.join(", ")}.`,
});
const unmigrated = (...folders) => ({
  regex: `^@/(${folders.join("|")})(/|$)`,
  message: `Shared and feature code must not import these folders: ${folders.join(", ")}.`,
});
const aliasOnly = [
  { regex: "(^|/)\\.\\.(/|$)", message: "Use the @/ alias instead of parent-relative imports." },
  { regex: "^src/", message: "Use the @/ alias instead of bare src/ imports." },
];
const boundary = (files, patterns, ignores = []) => ({
  files,
  ignores,
  rules: { "no-restricted-imports": ["error", { patterns: [...aliasOnly, ...patterns] }] },
});
const legacy = unmigrated("lib", "components", "routes");
const projectPath = "/^(@\\/|\\.|src\\/)/";

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
    { regex: "^@/features(/|$)", message: "Shared code must not import features." },
    legacy,
  ]),
  boundary(["src/features/issues/**/*.{ts,tsx}"], [features("auth", "repositories", "board", "analytics"), legacy]),
  boundary(["src/features/auth/**/*.{ts,tsx}"], [features("repositories", "board", "analytics"), legacy]),
  boundary(["src/features/repositories/**/*.{ts,tsx}"], [features("board", "analytics"), legacy]),
  boundary(["src/features/board/**/*.{ts,tsx}"], [features("auth", "analytics"), legacy]),
  boundary(["src/features/analytics/**/*.{ts,tsx}"], [features("auth", "board"), legacy]),
  {
    files: ["src/shared/**/*.{ts,tsx}", "src/features/**/*.{ts,tsx}"],
    rules: {
      "no-restricted-syntax": [
        "error",
        ...[`ImportExpression[source.value=${projectPath}]`, `TSImportType[argument.literal.value=${projectPath}]`].map((selector) => ({
          selector,
          message: "Use a static import so the boundary rules apply.",
        })),
      ],
    },
  },
);
