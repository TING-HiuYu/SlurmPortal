// UI language. Chinese source strings are the keys; locales/en.json maps them to English.
// The locale is read once at load: switching saves the choice and reloads, so labels built at
// module level (menus, status names) are translated too.
import en from "./locales/en.json";

const KEY = "gla.locale";
export const locales = [
  { value: "zh", label: "中文" },
  { value: "en", label: "English" },
];

function initial() {
  try {
    const v = localStorage.getItem(KEY);
    if (locales.some((l) => l.value === v)) return v;
  } catch {
    /* storage unavailable */
  }
  return "zh";
}

export const locale = initial();
// dateLocale formats dates and times in the chosen language.
export const dateLocale = locale === "en" ? "en-GB" : "zh-CN";
const dict = locale === "en" ? en : null;

// t returns the text for the current locale; {0}, {1}… are replaced by args.
export function t(key, ...args) {
  const s = dict && Object.hasOwn(dict, key) ? dict[key] : key;
  return args.length ? s.replace(/\{(\d+)\}/g, (m, i) => (i < args.length ? String(args[i]) : m)) : s;
}

export function setLocale(next) {
  if (next === locale) return;
  try {
    localStorage.setItem(KEY, next);
  } catch {
    /* storage unavailable: the choice lasts until reload only */
  }
  window.location.reload();
}
