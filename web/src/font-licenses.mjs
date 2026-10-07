// The fonts ship inside the binary, and the SIL Open Font License asks that
// each copy carries its copyright notice and the license. This writes them
// next to the font files, served at /static/dist/fonts/LICENSES.txt.
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";

const fonts = ["barlow", "barlow-condensed", "red-hat-mono"];
mkdirSync("../static/dist/fonts", { recursive: true });
const text = fonts
  .map((f) => `==== @fontsource/${f} ====\n\n` + readFileSync(`node_modules/@fontsource/${f}/LICENSE`, "utf8").trim())
  .join("\n\n");
writeFileSync("../static/dist/fonts/LICENSES.txt", text + "\n");
