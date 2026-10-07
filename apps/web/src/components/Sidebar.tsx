'use client';

import Link from 'next/link';
import { usePathname, useRouter } from 'next/navigation';
import {
  LayoutDashboard,
  Wallet,
  ArrowLeftRight,
  KeyRound,
  Webhook,
  BarChart3,
  Users,
  LogOut,
  Coins,
  Layers,
  Calendar,
  Banknote,
  Link2,
} from 'lucide-react';
import { useAuth } from '@/lib/auth-context';
import { cn } from '@/lib/utils';

const navItems = [
  { name: 'Overview', href: '/overview', icon: LayoutDashboard },
  { name: 'Wallets', href: '/wallets', icon: Wallet },
  { name: 'Transfers', href: '/transfers', icon: ArrowLeftRight },
  { name: 'Batch', href: '/batch', icon: Layers },
  { name: 'Schedules', href: '/schedules', icon: Calendar },
  { name: 'FX', href: '/fx', icon: Coins },
  { name: 'Conversions', href: '/conversions', icon: Coins },
  { name: 'Fiat', href: '/fiat', icon: Banknote },
  { name: 'Payment Links', href: '/payment-links', icon: Link2 },
  { name: 'API Keys', href: '/api-keys', icon: KeyRound },
  { name: 'Webhooks', href: '/webhooks', icon: Webhook },
  { name: 'Usage', href: '/usage', icon: BarChart3 },
  { name: 'Team', href: '/team', icon: Users },
];

export default function Sidebar() {
  const pathname = usePathname();
  const { logout } = useAuth();
  const router = useRouter();

  const handleLogout = () => {
    logout();
    router.push('/login');
  };

  return (
    <aside className="fixed inset-y-0 left-0 z-50 flex w-64 flex-col border-r border-border bg-surface">
      <div className="flex h-16 items-center gap-2 px-6">
        <div className="flex h-8 w-8 items-center justify-center rounded-lg bg-primary text-primary-foreground">
          <span className="text-sm font-bold">F</span>
        </div>
        <div className="flex items-baseline gap-2">
          <span className="text-lg font-semibold tracking-tight text-foreground">Nexora</span>
          <span className="rounded border border-border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
            Tenant
          </span>
        </div>
      </div>

      <nav className="flex flex-1 flex-col gap-1 px-4 py-4">
        {navItems.map((item) => {
          const Icon = item.icon;
          const isActive = pathname?.startsWith(item.href);
          return (
            <Link
              key={item.name}
              href={item.href}
              className={cn(
                'group flex items-center gap-3 rounded-lg px-3 py-2.5 text-sm font-medium transition-colors',
                isActive
                  ? 'bg-primary-subtle text-primary'
                  : 'text-muted-foreground hover:bg-muted hover:text-foreground',
              )}
            >
              <Icon
                className={cn(
                  'h-5 w-5 transition-colors',
                  isActive ? 'text-primary' : 'text-muted-foreground group-hover:text-foreground',
                )}
              />
              {item.name}
            </Link>
          );
        })}
      </nav>

      <div className="border-t border-border p-4">
        <button
          onClick={handleLogout}
          className="flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-sm font-medium text-muted-foreground transition-colors hover:bg-danger-subtle hover:text-danger"
        >
          <LogOut className="h-5 w-5" />
          Logout
        </button>
      </div>
    </aside>
  );
}
