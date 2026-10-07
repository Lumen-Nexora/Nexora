'use client';

import { useEffect, useState, useCallback } from 'react';
import { api, type APIKey } from '@/lib/api';
import { useToast } from '@/lib/toast-context';
import { PageHeader } from '@/components/ui/page-header';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Select } from '@/components/ui/select';
import { Card } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import { Modal } from '@/components/ui/modal';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { EmptyState } from '@/components/ui/empty-state';
import { Skeleton } from '@/components/ui/skeleton';
import { KeyRound, Plus, Copy, Trash2, RefreshCw } from 'lucide-react';

export default function ApiKeysPage() {
  const { toast } = useToast();
  const [keys, setKeys] = useState<APIKey[]>([]);
  const [loading, setLoading] = useState(true);
  const [creating, setCreating] = useState(false);
  const [rotatingId, setRotatingId] = useState<string | null>(null);
  const [newKey, setNewKey] = useState<string | null>(null);
  const [modalTitle, setModalTitle] = useState('API Key Created');
  const [modalDescription, setModalDescription] = useState(
    'Please copy this key and save it somewhere safe. For security reasons, we cannot show it to you again.',
  );
  const [label, setLabel] = useState('');
  const [mode, setMode] = useState<'live' | 'test'>(() =>
    typeof window !== 'undefined' && window.localStorage.getItem('nexora_mode') === 'test'
      ? 'test'
      : 'live',
  );
  const [expiryDays, setExpiryDays] = useState('90');
  const [reminderDays, setReminderDays] = useState('7');
  const [mountedAt, setMountedAt] = useState<number | null>(null);

  const fetchKeys = useCallback(async () => {
    setLoading(true);
    try {
      const data = await api.listAPIKeys();
      setKeys(Array.isArray(data) ? data : []);
      setMountedAt(Date.now());
    } catch (err) {
      toast(err instanceof Error ? err.message : 'Failed to load API keys', 'error');
    } finally {
      setLoading(false);
    }
  }, [toast]);

  useEffect(() => {
    let cancelled = false;
    const run = async () => {
      if (cancelled) return;
      await fetchKeys();
    };
    run();
    return () => {
      cancelled = true;
    };
  }, [fetchKeys]);

  const handleCreateKey = async () => {
    setCreating(true);
    try {
      let expiresAt: string | undefined = undefined;
      const days = parseInt(expiryDays, 10);
      if (!isNaN(days) && days > 0) {
        const exp = new Date();
        exp.setDate(exp.getDate() + days);
        expiresAt = exp.toISOString();
      }

      const res = await api.createAPIKey({
        label: label || undefined,
        mode,
        expires_at: expiresAt,
        rotation_reminder_days: parseInt(reminderDays, 10) || 7,
      });

      setModalTitle('API Key Created');
      setModalDescription(
        'Please copy this key and save it somewhere safe. For security reasons, we cannot show it to you again.',
      );
      setNewKey(res.key);
      setLabel('');
      toast('API key created', 'success');
      await fetchKeys();
    } catch (err) {
      toast(err instanceof Error ? err.message : 'Failed to create key', 'error');
    } finally {
      setCreating(false);
    }
  };

  const handleRotate = async (id: string) => {
    if (
      !confirm(
        'Rotating this API key will immediately revoke the current key and generate a fresh one with the same permissions. Proceed?',
      )
    ) {
      return;
    }
    setRotatingId(id);
    try {
      const res = await api.rotateAPIKey(id);
      setModalTitle('API Key Rotated');
      setModalDescription(
        'The previous key has been revoked. Copy the new key below and update your services immediately.',
      );
      setNewKey(res.key);
      toast('API key rotated successfully', 'success');
      await fetchKeys();
    } catch (err) {
      toast(err instanceof Error ? err.message : 'Failed to rotate key', 'error');
    } finally {
      setRotatingId(null);
    }
  };

  const handleRevoke = async (id: string) => {
    if (!confirm('Are you sure you want to revoke this key?')) return;
    try {
      await api.revokeAPIKey(id);
      toast('API key revoked', 'success');
      await fetchKeys();
    } catch (err) {
      toast(err instanceof Error ? err.message : 'Failed to revoke key', 'error');
    }
  };

  const copyKey = () => {
    if (newKey) {
      navigator.clipboard.writeText(newKey);
      toast('Key copied to clipboard', 'success');
    }
  };

  const renderExpiry = (k: APIKey) => {
    if (!k.expires_at) {
      return <span className="text-muted-foreground text-xs">Never</span>;
    }

    const expTime = new Date(k.expires_at).getTime();
    const referenceTime = mountedAt ?? 0;
    const diffDays =
      referenceTime > 0 ? Math.ceil((expTime - referenceTime) / (1000 * 60 * 60 * 24)) : 0;
    const isExpired = k.is_expired || (referenceTime > 0 && diffDays <= 0);

    if (isExpired) {
      return (
        <Badge variant="danger" className="text-xs">
          Expired
        </Badge>
      );
    }

    const reminderThreshold = k.rotation_reminder_days ?? 7;
    const isSoon = diffDays <= reminderThreshold;

    return (
      <div className="flex flex-col gap-0.5">
        <span className="text-xs font-medium">{new Date(k.expires_at).toLocaleDateString()}</span>
        {isSoon ? (
          <span className="text-[11px] font-semibold text-warning">Expires in {diffDays}d</span>
        ) : (
          <span className="text-[11px] text-muted-foreground">In {diffDays} days</span>
        )}
      </div>
    );
  };

  if (loading) {
    return (
      <div className="flex flex-col gap-8">
        <div className="flex items-center justify-between">
          <Skeleton className="h-10 w-48" />
          <Skeleton className="h-10 w-40" />
        </div>
        <Skeleton className="h-64" />
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-8 animate-in fade-in slide-in-from-bottom-4 duration-500">
      <PageHeader
        title="API Keys"
        description="Manage your API keys, rotation policies, and expiration reminders."
      >
        <div className="flex flex-wrap items-center gap-3">
          <Input
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            placeholder="Key label (optional)"
            className="w-44"
          />
          <Select
            value={mode}
            onChange={(e) => setMode(e.target.value as 'live' | 'test')}
            className="w-36"
            aria-label="API key environment"
          >
            <option value="live">Live · mainnet</option>
            <option value="test">Test · testnet</option>
          </Select>
          <Select
            value={expiryDays}
            onChange={(e) => setExpiryDays(e.target.value)}
            className="w-36"
            aria-label="Key Expiry Policy"
          >
            <option value="30">Expires in 30d</option>
            <option value="60">Expires in 60d</option>
            <option value="90">Expires in 90d</option>
            <option value="365">Expires in 1y</option>
            <option value="0">Never Expire</option>
          </Select>
          <Select
            value={reminderDays}
            onChange={(e) => setReminderDays(e.target.value)}
            className="w-36"
            aria-label="Rotation Reminder Window"
          >
            <option value="7">Remind 7d before</option>
            <option value="14">Remind 14d before</option>
            <option value="30">Remind 30d before</option>
          </Select>
          <Button onClick={handleCreateKey} isLoading={creating}>
            <Plus className="h-4 w-4" />
            Create Secret Key
          </Button>
        </div>
      </PageHeader>

      <Modal
        open={!!newKey}
        onClose={() => setNewKey(null)}
        title={modalTitle}
        description={modalDescription}
      >
        <div className="flex flex-col gap-5">
          <div className="flex items-center justify-between gap-3 rounded-xl border border-primary/20 bg-primary-subtle p-4">
            <code className="break-all font-mono text-sm text-foreground">{newKey}</code>
            <Button variant="secondary" size="sm" onClick={copyKey}>
              <Copy className="h-4 w-4" />
              Copy
            </Button>
          </div>
          <Button onClick={() => setNewKey(null)} className="w-full">
            I have saved my key
          </Button>
        </div>
      </Modal>

      <Card>
        {keys.length === 0 ? (
          <EmptyState
            icon={KeyRound}
            title="No API keys yet"
            description="Create your first key to authenticate API requests."
          />
        ) : (
          <Table>
            <TableHead>
              <TableRow>
                <TableHeader>Name</TableHeader>
                <TableHeader>Environment</TableHeader>
                <TableHeader>Token</TableHeader>
                <TableHeader>Created</TableHeader>
                <TableHeader>Expires</TableHeader>
                <TableHeader>Last Used</TableHeader>
                <TableHeader>Status</TableHeader>
                <TableHeader className="text-right">Actions</TableHeader>
              </TableRow>
            </TableHead>
            <TableBody>
              {keys.map((k) => {
                const isRevoked = !!k.revoked_at;
                const isExpired =
                  k.is_expired ||
                  (mountedAt !== null && k.expires_at
                    ? new Date(k.expires_at).getTime() <= mountedAt
                    : false);
                return (
                  <TableRow key={k.id} className={isRevoked ? 'opacity-60' : undefined}>
                    <TableCell className="font-medium">{k.label || 'Unnamed Key'}</TableCell>
                    <TableCell>
                      <Badge variant={k.mode === 'test' ? 'warning' : 'default'}>{k.mode}</Badge>
                    </TableCell>
                    <TableCell>
                      <code className="rounded-md border border-border bg-muted px-2 py-1 font-mono text-xs">
                        {k.prefix}••••••••••••
                      </code>
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {new Date(k.created_at).toLocaleDateString()}
                    </TableCell>
                    <TableCell>{renderExpiry(k)}</TableCell>
                    <TableCell className="text-muted-foreground">
                      {k.last_used_at ? new Date(k.last_used_at).toLocaleDateString() : 'Never'}
                    </TableCell>
                    <TableCell>
                      {isRevoked ? (
                        <Badge variant="default">Revoked</Badge>
                      ) : isExpired ? (
                        <Badge variant="danger">Expired</Badge>
                      ) : (
                        <Badge variant="success">Active</Badge>
                      )}
                    </TableCell>
                    <TableCell className="text-right">
                      {!isRevoked && (
                        <div className="flex items-center justify-end gap-2">
                          <Button
                            variant="ghost"
                            size="sm"
                            title="Rotate this API key"
                            isLoading={rotatingId === k.id}
                            onClick={() => handleRotate(k.id)}
                          >
                            <RefreshCw className="h-4 w-4" />
                            Rotate
                          </Button>
                          <Button
                            variant="ghost"
                            size="sm"
                            className="text-danger hover:bg-danger-subtle hover:text-danger"
                            onClick={() => handleRevoke(k.id)}
                          >
                            <Trash2 className="h-4 w-4" />
                            Revoke
                          </Button>
                        </div>
                      )}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  );
}
