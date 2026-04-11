# Third-party licenses

usage-buttons bundles assets and reference material from other open
source projects. Their licenses are reproduced / referenced below as
required.

## CodexBar — MIT

<https://github.com/steipete/CodexBar>

Copyright (c) 2026 Peter Steinberger

The following files are adapted from CodexBar's
`Sources/CodexBar/Resources/ProviderIcon-*.svg` assets, which are
distributed under the MIT license:

- `src/providers/provider-icons.generated.ts` — the `d` attribute
  of each provider's single-path SVG is embedded into a
  TypeScript map for compile-time inlining by
  `bun build --compile`. The actual SVG paths are unmodified from
  upstream; only the surrounding module structure is ours.

- The provider-branding color table in
  `src/providers/brand-colors.ts` mirrors the RGB values from
  CodexBar's `<Name>ProviderDescriptor.swift` `branding` blocks.

Full MIT license text is reproduced in `tmp/CodexBar/LICENSE` when
the CodexBar reference clone is present. Authoritative source:
<https://github.com/steipete/CodexBar/blob/main/LICENSE>

Permission is hereby granted, free of charge, to any person obtaining
a copy of this software and associated documentation files (the
"Software"), to deal in the Software without restriction, including
without limitation the rights to use, copy, modify, merge, publish,
distribute, sublicense, and/or sell copies of the Software, and to
permit persons to whom the Software is furnished to do so, subject to
the condition that the above copyright notice and this permission
notice be included in all copies or substantial portions of the
Software. The Software is provided "AS IS", without warranty of any
kind.

## @steipete/sweet-cookie — MIT

<https://github.com/steipete/sweet-cookie>

Copyright (c) 2026 Peter Steinberger

Runtime dependency declared in `package.json`. Not modified. Used
via its public `getCookies()` / `toCookieHeader()` API for
claude.ai session cookie extraction on Windows. License travels
with the package itself in `node_modules/@steipete/sweet-cookie`.
