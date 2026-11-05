import axios from "axios";

import { clearSession, getToken } from "./session";

export interface ApiResponse<T> {
  ok: boolean;
  data: T;
  message: string;
}

export interface SystemInfo {
  app_name: string;
  app_version: string;
  api_prefix: string;
  mode: string;
}

export const API_BASE_URL = import.meta.env.VITE_API_BASE_URL || "";

export const api = axios.create({
  baseURL: API_BASE_URL,
  timeout: 15000
});

api.interceptors.request.use((config) => {
  const token = getToken();
  if (token) {
    config.headers.Authorization = `Bearer ${token}`;
  }
  return config;
});

api.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error?.response?.status === 401) {
      clearSession();
      if (window.location.pathname !== "/login") {
        window.location.href = "/login";
      }
    }
    return Promise.reject(error);
  }
);

export async function fetchSystemInfo(): Promise<SystemInfo> {
  const response = await api.get<ApiResponse<SystemInfo>>("/api/v1/system/info");
  return response.data.data;
}

export async function fetchHealth(): Promise<{ status: string }> {
  const response = await api.get<{ status: string }>("/healthz");
  return response.data;
}

// LLM administration has a different trust boundary and credential from sensor ops.
export function getManagerToken(): string {
  return sessionStorage.getItem("alterhive_manager_token") || "";
}

export function setManagerToken(value: string) {
  if (value) sessionStorage.setItem("alterhive_manager_token", value);
  else sessionStorage.removeItem("alterhive_manager_token");
}

export const managerApi = axios.create({
  baseURL: import.meta.env.VITE_LLM_API_BASE_URL || "/manager",
  timeout: 15000
});

managerApi.interceptors.request.use((config) => {
  const token = getManagerToken();
  if (!token) throw new Error("请单独输入 manager 运维 token；不会复用 sensor 登录凭据。");
  config.headers.Authorization = `Bearer ${token}`;
  return config;
});

managerApi.interceptors.response.use(
  response => response,
  error => {
    if (error?.response?.status === 401) setManagerToken("");
    return Promise.reject(error);
  }
);
