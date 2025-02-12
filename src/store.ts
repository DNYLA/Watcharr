import { writable } from "svelte/store";
import type { Theme, Watched } from "./types";
import type { Notification } from "./lib/util/notify";
import { browser } from "$app/environment";
import { toggleTheme } from "./lib/util/helpers";

export const watchedList = writable<Watched[]>([]);
export const notifications = writable<(Notification & { id: number })[]>([]);
export const activeFilter = writable<string[]>(["DATEADDED", "DOWN"]);
export const appTheme = writable<Theme>();

export const clearAllStores = () => {
  watchedList.set([]);
  notifications.set([]);
  activeFilter.set(["DATEADDED", "DOWN"]);
};

if (browser) {
  // Rehydrate
  const raf = localStorage.getItem("activeFilter");
  if (raf) {
    activeFilter.update((v) => (v = JSON.parse(raf)));
  }

  const theme = localStorage.getItem("theme") as Theme;
  if (theme) {
    appTheme.update((t) => (t = theme));
    toggleTheme(theme);
  } else {
    let defTheme: Theme = "light";
    if (window.matchMedia("(prefers-color-scheme: dark)").matches) {
      defTheme = "dark";
    }
    console.log("Theme not set, setting default theme from system theme:", defTheme);
    appTheme.update((t) => (t = defTheme));
    toggleTheme(defTheme);
  }

  // Save changes
  activeFilter.subscribe((v) => {
    localStorage.setItem("activeFilter", JSON.stringify(v));
  });

  appTheme.subscribe((v) => {
    localStorage.setItem("theme", v);
  });
}
