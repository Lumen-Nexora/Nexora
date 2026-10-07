'use client';

import { useState } from 'react';
import { useRouter } from 'next/navigation';
import { useAuth } from '@/lib/auth-context';
import { api } from '@/lib/api';
import { useToast } from '@/lib/toast-context';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';

export default function LoginPage() {
  const [apiKey, setApiKey] = useState('');
  const [isLoading, setIsLoading] = useState(false);
  const [error, setError] = useState('');
  const { login } = useAuth();
  const router = useRouter();
  const { toast } = useToast();

  const handleLogin = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setIsLoading(true);

    try {
      login(apiKey);
      await api.getHealth();
      toast('Connected to Nexora API', 'success');
      router.push('/overview');
    } catch {
      setError('Invalid API key or API unreachable');
      localStorage.removeItem('nexora_api_key');
    } finally {
      setIsLoading(false);
    }
  };

  return (
    <div className="flex min-h-screen items-center justify-center p-6">
      <Card className="w-full max-w-[420px] animate-in fade-in slide-in-from-bottom-4 duration-500">
        <CardHeader className="text-center">
          <div className="mx-auto flex h-10 w-10 items-center justify-center rounded-lg bg-primary text-primary-foreground">
            <span className="font-bold">F</span>
          </div>
          <CardTitle className="text-xl">Sign in to Nexora</CardTitle>
          <CardDescription>Enter your secret API key to access the dashboard.</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleLogin} className="flex flex-col gap-5">
            <div className="flex flex-col gap-1.5">
              <label htmlFor="apiKey" className="text-sm font-medium text-foreground">
                API Key
              </label>
              <Input
                id="apiKey"
                type="password"
                value={apiKey}
                onChange={(e) => setApiKey(e.target.value)}
                required
                placeholder="sk_..."
                className="font-mono"
              />
            </div>

            {error && (
              <p className="rounded-lg bg-danger-subtle px-3 py-2 text-sm text-danger">{error}</p>
            )}

            <Button
              type="submit"
              size="lg"
              className="w-full"
              isLoading={isLoading}
              disabled={!apiKey}
            >
              {isLoading ? 'Connecting...' : 'Sign In'}
            </Button>
          </form>

          <p className="mt-6 text-center text-xs text-muted-foreground">
            API keys are managed via the Nexora API or CLI.
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
