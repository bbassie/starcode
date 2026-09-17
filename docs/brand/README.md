# Brand images

The mark is the Lucide `star` icon the sidebar shows next to the name,
stroked in the accent color `#8497bd`.

- `icon.svg`, `icon-512.png`: square icon for a GitHub avatar or anywhere
  a 512x512 image is asked for. GitHub rounds the corners itself.
- `social-preview.svg`, `social-preview.png`: 1280x640 card for the repo's
  social preview (Settings, General, Social preview).

The app's own favicon and maskable icon live in `internal/web/static`.
The PNGs were rendered from the SVGs in headless Chromium; the social
preview SVG names a font stack, so re-render from it rather than using it
directly on a site where the fonts differ.
