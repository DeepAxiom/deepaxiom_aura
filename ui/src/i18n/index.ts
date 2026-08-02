import i18n from "i18next";
import { initReactI18next } from "react-i18next";
import en from "./en.json";
import es from "./es.json";

// English is the base language. Adding a locale = adding one JSON file here.
i18n.use(initReactI18next).init({
  resources: {
    en: { translation: en },
    es: { translation: es },
  },
  lng: localStorage.getItem("aura.lang") ?? "en",
  fallbackLng: "en",
  interpolation: { escapeValue: false },
});

export function setLanguage(lang: string) {
  localStorage.setItem("aura.lang", lang);
  void i18n.changeLanguage(lang);
}

export const LANGUAGES = [
  { code: "en", label: "English" },
  { code: "es", label: "Español" },
];

export default i18n;
