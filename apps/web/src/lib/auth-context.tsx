'use client';

import { createContext, useContext, useState, useCallback, type ReactNode } from 'react';

export type EnvironmentMode = 'live' | 'test';

interface AuthContextValue {
  apiKey: string | null;
  mode: EnvironmentMode;
  isAuthenticated: boolean;
  login: (apiKey: string) => void;
  logout: () => void;
  switchMode: (mode: EnvironmentMode) => void;
  getStoredWalletIds: () => string[];
  addStoredWalletId: (id: string) => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);
const WALLET_IDS_KEY = 'nexora_wallet_ids';
const MODE_KEY = 'nexora_mode';

function readMode(): EnvironmentMode {
  if (typeof window === 'undefined') return 'live';
  return window.localStorage.getItem(MODE_KEY) === 'test' ? 'test' : 'live';
}

function keyForMode(mode: EnvironmentMode): string | null {
  if (typeof window === 'undefined') return null;
  return window.localStorage.getItem(`nexora_api_key_${mode}`);
}

function modeForKey(key: string): EnvironmentMode {
  return key.startsWith('sk_test_') ? 'test' : 'live';
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [mode, setMode] = useState<EnvironmentMode>(() => readMode());
  const [apiKey, setApiKey] = useState<string | null>(() => {
    if (typeof window === 'undefined') return null;
    const storedMode = readMode();
    return keyForMode(storedMode) || window.localStorage.getItem('nexora_api_key');
  });

  const login = useCallback((key: string) => {
    const nextMode = modeForKey(key);
    window.localStorage.setItem(`nexora_api_key_${nextMode}`, key);
    window.localStorage.setItem('nexora_api_key', key);
    window.localStorage.setItem(MODE_KEY, nextMode);
    setMode(nextMode);
    setApiKey(key);
  }, []);

  const logout = useCallback(() => {
    const currentMode = readMode();
    window.localStorage.removeItem(`nexora_api_key_${currentMode}`);
    window.localStorage.removeItem('nexora_api_key');
    window.localStorage.removeItem(WALLET_IDS_KEY);
    setApiKey(null);
  }, []);

  const switchMode = useCallback((nextMode: EnvironmentMode) => {
    window.localStorage.setItem(MODE_KEY, nextMode);
    const nextKey = keyForMode(nextMode);
    if (nextKey) window.localStorage.setItem('nexora_api_key', nextKey);
    else window.localStorage.removeItem('nexora_api_key');
    setMode(nextMode);
    setApiKey(nextKey);
  }, []);

  const getStoredWalletIds = useCallback((): string[] => {
    try {
      const raw = window.localStorage.getItem(`${WALLET_IDS_KEY}_${mode}`);
      return raw ? JSON.parse(raw) : [];
    } catch {
      return [];
    }
  }, [mode]);

  const addStoredWalletId = useCallback(
    (id: string) => {
      const existing = getStoredWalletIds();
      if (!existing.includes(id)) {
        window.localStorage.setItem(`${WALLET_IDS_KEY}_${mode}`, JSON.stringify([...existing, id]));
      }
    },
    [getStoredWalletIds, mode],
  );

  return (
    <AuthContext.Provider
      value={{
        apiKey,
        mode,
        isAuthenticated: !!apiKey,
        login,
        logout,
        switchMode,
        getStoredWalletIds,
        addStoredWalletId,
      }}
    >
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth must be used within AuthProvider');
  return ctx;
}
