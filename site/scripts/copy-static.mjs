// Copies files kept in docs/ that the site serves as they are (the interactive architecture
// diagram) into static/ before start and build. The copies are not committed.
import { copyFileSync } from "node:fs";

for (const file of ["architecture.html"]) {
  copyFileSync(new URL(`../../docs/${file}`, import.meta.url), new URL(`../static/${file}`, import.meta.url));
}
