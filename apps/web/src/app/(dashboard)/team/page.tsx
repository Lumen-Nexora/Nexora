'use client';

import { useState } from 'react';
import { useToast } from '@/lib/toast-context';
import { PageHeader } from '@/components/ui/page-header';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Select } from '@/components/ui/select';
import { Card } from '@/components/ui/card';
import { Modal } from '@/components/ui/modal';
import { EmptyState } from '@/components/ui/empty-state';
import { Users, Plus } from 'lucide-react';

export default function TeamPage() {
  const { toast } = useToast();
  const [isInviting, setIsInviting] = useState(false);
  const [inviteEmail, setInviteEmail] = useState('');
  const [inviteRole, setInviteRole] = useState('developer');

  const handleInvite = (e: React.FormEvent) => {
    e.preventDefault();
    toast('Team management is not yet available via the API', 'info');
    setIsInviting(false);
    setInviteEmail('');
  };

  return (
    <div className="flex flex-col gap-8 animate-in fade-in slide-in-from-bottom-4 duration-500">
      <PageHeader title="Team Management" description="Manage organization members and roles.">
        <Button onClick={() => setIsInviting(true)}>
          <Plus className="h-4 w-4" />
          Invite Member
        </Button>
      </PageHeader>

      <Modal
        open={isInviting}
        onClose={() => setIsInviting(false)}
        title="Invite Team Member"
        description="Send an invitation to join your organization."
      >
        <form onSubmit={handleInvite} className="flex flex-col gap-5">
          <div className="flex flex-col gap-1.5">
            <label className="text-sm font-medium text-foreground">Email Address</label>
            <Input
              type="email"
              value={inviteEmail}
              onChange={(e) => setInviteEmail(e.target.value)}
              required
              placeholder="colleague@example.com"
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <label className="text-sm font-medium text-foreground">Role</label>
            <Select value={inviteRole} onChange={(e) => setInviteRole(e.target.value)}>
              <option value="developer">Developer</option>
              <option value="finance">Finance</option>
              <option value="admin">Admin</option>
            </Select>
          </div>
          <div className="flex justify-end gap-3">
            <Button type="button" variant="secondary" onClick={() => setIsInviting(false)}>
              Cancel
            </Button>
            <Button type="submit">Send Invite</Button>
          </div>
        </form>
      </Modal>

      <Card>
        <EmptyState
          icon={Users}
          title="Team management is coming soon"
          description="User and role management will be available via the Nexora API."
        />
      </Card>
    </div>
  );
}
