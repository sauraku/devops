import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import * as api from '../lib/api';
import type { Project, Backup } from '../types';
import { Archive, Plus, RotateCcw, ShieldCheck, Loader2, Database, X } from 'lucide-react';
import { useToast } from './Toast';

interface BackupsProps {
  project: Project;
}

interface TableInfo {
  name: string;
  rows: string;
  kind: string;
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function formatTime(ts: string): string {
  try { return new Date(ts).toLocaleString(); } catch { return ts; }
}

function RestoreConfirmModal({ open, backup, tables, isLoading, confirmText, onConfirmTextChange, onConfirm, onClose }: {
  open: boolean;
  backup: Backup | null;
  tables: TableInfo[];
  isLoading: boolean;
  confirmText: string;
  onConfirmTextChange: (v: string) => void;
  onConfirm: () => void;
  onClose: () => void;
}) {
  if (!open || !backup) return null;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-sm" onClick={onClose}>
      <div className="bg-surface border border-line-strong rounded-2xl p-6 w-full max-w-lg max-h-[80vh] flex flex-col shadow-2xl" onClick={e => e.stopPropagation()}>
        <div className="flex items-center justify-between mb-4">
          <h2 className="text-sm font-black uppercase tracking-wider text-bad flex items-center gap-2">
            <Database size={16} /> Restore Database
          </h2>
          <button onClick={onClose} className="text-muted hover:text-ink p-1 rounded hover:bg-surface-2 transition-colors">
            <X size={18} />
          </button>
        </div>

        <div className="mb-4 p-3 rounded-xl bg-bad/5 border border-bad/20 text-xs text-bad">
          <p className="font-bold mb-1">DANGER: This will overwrite your current database.</p>
          <p>Backup: <code className="text-ink font-mono">{backup.id}</code> · {formatBytes(backup.size_bytes)}</p>
        </div>

        {isLoading ? (
          <div className="flex items-center justify-center py-8 gap-2 text-muted">
            <Loader2 size={16} className="animate-spin text-accent" /> Analyzing backup contents...
          </div>
        ) : tables.length > 0 ? (
            <div className="flex-1 overflow-hidden flex flex-col min-h-0 mb-4">
              <p className="text-[10px] font-bold uppercase tracking-wider text-muted mb-2">
                DB Diff ({tables.length} tables) — <span className="text-bad">red=deleted</span> <span className="text-warn">amber=overwritten</span> <span className="text-good">green=new</span>
              </p>
              <div className="flex-1 overflow-y-auto border border-line rounded-xl bg-surface-2">
                <table className="w-full text-left text-[11px]">
                  <thead className="sticky top-0 bg-surface-3 text-muted uppercase tracking-wider font-bold">
                    <tr>
                      <th className="py-2 px-3 text-left">Table</th>
                      <th className="py-2 px-3 text-right w-20">Current Rows</th>
                      <th className="py-2 px-3 text-left w-24">Action</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-line/40 font-mono">
                    {tables.map((t, i) => {
                        const isDrop = t.kind === 'dropped';
                        const isNew = t.kind === 'new';
                      return (
                        <tr key={i} className={`hover:bg-surface-3/30 ${isDrop ? 'bg-bad/5' : isNew ? 'bg-good/5' : ''}`}>
                          <td className="py-1.5 px-3 text-ink-soft">{t.name}</td>
                          <td className={`py-1.5 px-3 text-right ${isDrop ? 'text-bad' : 'text-ink-soft'}`}>{t.rows}</td>
                          <td className={`py-1.5 px-3 text-[10px] font-bold uppercase ${
                            isDrop ? 'text-bad' : isNew ? 'text-good' : 'text-warn'
                          }`}>
                            {t.kind}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            </div>
        ) : (
          <p className="text-xs text-muted mb-4">Table listing unavailable — restore will proceed with full database.</p>
        )}

        <div className="border-t border-line pt-4">
          <label className="block text-[10px] font-bold uppercase tracking-wider text-muted mb-2">
            Type <code className="text-bad font-mono">restore</code> to confirm
          </label>
          <input
            value={confirmText}
            onChange={e => onConfirmTextChange(e.target.value)}
            placeholder="restore"
            className="w-full px-3 py-2 rounded-xl bg-surface-2 border border-line text-ink text-sm font-mono focus:outline-none focus:border-bad/50 transition-colors"
            autoFocus
            onKeyDown={e => { if (e.key === 'Enter' && confirmText === 'restore') onConfirm(); }}
          />
          <button
            onClick={onConfirm}
            disabled={confirmText !== 'restore'}
            className="w-full mt-3 py-2.5 rounded-xl bg-bad text-white font-bold text-xs uppercase tracking-wider disabled:opacity-30 hover:brightness-110 transition-all"
          >
            Confirm Restore
          </button>
        </div>
      </div>
    </div>
  );
}

export function Backups({ project }: BackupsProps) {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const [branch, setBranch] = useState(project.branch_name);
  const [reason, setReason] = useState('');
  const [copied, setCopied] = useState<string | null>(null);
  const [restoreModal, setRestoreModal] = useState<{backup: Backup} | null>(null);
  const [restoreConfirm, setRestoreConfirm] = useState('');
  const [dryRunTables, setDryRunTables] = useState<TableInfo[]>([]);
  const [dryRunLoading, setDryRunLoading] = useState(false);

  const { data: backupsData, isLoading, error: queryError } = useQuery({
    queryKey: ['backups', project.id],
    queryFn: () => api.getBackups(project.id),
    refetchInterval: 15000,
  });

  const backups: Backup[] = backupsData?.backups ?? [];

  const createMutation = useMutation({
    mutationFn: () => api.createBackup(project.id, branch, reason),
    onSuccess: () => { 
      queryClient.invalidateQueries({ queryKey: ['backups', project.id] }); 
      toast('Backup build triggered successfully.', 'success'); 
      setReason(''); 
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const verifyMutation = useMutation({
    mutationFn: (backupId: string) => api.verifyBackup(project.id, backupId),
    onSuccess: (data) => { 
      toast(data.message, data.ok ? 'success' : 'error'); 
      queryClient.invalidateQueries({ queryKey: ['backups', project.id] }); 
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const restoreMutation = useMutation({
    mutationFn: (backupId: string) => api.restoreBackup(project.id, backupId),
    onSuccess: (data) => { 
      toast(`System restore initiated: ${data.operation?.id ?? 'unknownRelease'}`, 'success'); 
      queryClient.invalidateQueries({ queryKey: ['project-status', project.id] });
      setRestoreModal(null);
      setRestoreConfirm('');
      setDryRunTables([]);
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const handleRestoreClick = async (backup: Backup) => {
    setRestoreModal({ backup });
    setRestoreConfirm('');
    setDryRunLoading(true);
    setDryRunTables([]);
    try {
      const res = await api.dryRunRestore(project.id, backup.id);
      setDryRunTables(res.tables ?? []);
    } catch {
      setDryRunTables([]);
    } finally {
      setDryRunLoading(false);
    }
  };

  const handleRestoreConfirm = () => {
    if (!restoreModal) return;
    restoreMutation.mutate(restoreModal.backup.id);
  };

  return (
    <div className="max-w-3xl space-y-6">

      {/* Backup Builder Control Strip */}
      <div className="glass-panel rounded-2xl p-6 border border-line shadow-lg">
        <h3 className="text-xs font-bold uppercase tracking-wider text-ink mb-4 flex items-center gap-2">
          <Plus size={14} className="text-accent" /> Create New Snapshot
        </h3>
        <form onSubmit={(e) => { e.preventDefault(); createMutation.mutate(); }} className="flex flex-col sm:flex-row items-stretch sm:items-end gap-4">
          <div className="flex-1">
            <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Target Branch</label>
            <input 
              value={branch} 
              onChange={(e) => setBranch(e.target.value)} 
              className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all font-mono" 
            />
          </div>
          <div className="flex-1">
            <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Justification / Reason</label>
            <input 
              value={reason} 
              onChange={(e) => setReason(e.target.value)} 
              placeholder="e.g., prior to v2 database migrations" 
              className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all" 
            />
          </div>
          <button 
            type="submit" 
            disabled={createMutation.isPending} 
            className="px-5 py-2.5 rounded-xl bg-gradient-to-r from-accent to-accent-hover hover:scale-[1.02] text-accent-on font-bold text-xs transition-all disabled:opacity-40 disabled:scale-100 flex items-center justify-center gap-1.5 shadow-[0_4px_15px_-3px_var(--accent)]/30 h-[38px] shrink-0 active:scale-[0.98]"
          >
            <Plus size={14} className="stroke-[3]" /> 
            {createMutation.isPending ? 'CREATING...' : 'CREATE'}
          </button>
        </form>
      </div>

      {/* Monday.dev Board Group: Backups Table */}
      <div className="glass-panel rounded-2xl border border-line overflow-hidden shadow-xl">
        <div className="px-5 py-4 bg-surface-2/30 border-b border-line flex items-center justify-between">
          <div className="flex items-center gap-3">
            <div className="p-1 rounded bg-accent/10 border-accent/25 text-accent">
              <Archive size={14} />
            </div>
            <h2 className="text-xs font-bold tracking-wider uppercase text-ink">
              System Release Backups & Snapshots
              <span className="text-[10px] text-muted normal-case font-medium ml-2">({backups.length} snapshots stored)</span>
            </h2>
          </div>
        </div>

        <div className="overflow-x-auto">
          {isLoading ? (
            <div className="flex items-center justify-center py-12 gap-2 text-xs text-muted font-bold uppercase tracking-wider">
              <Loader2 size={16} className="animate-spin text-accent" /> Loading Snapshot Repositories...
            </div>
          ) : queryError ? (
            <div className="p-8 text-center text-xs font-extrabold text-bad">
              Failed to query database backup catalogs: {(queryError as Error).message}
            </div>
          ) : backups.length === 0 ? (
            <div className="p-12 text-center text-xs text-muted">
              No backups are currently registered under this project scope.
            </div>
          ) : (
            <table className="w-full text-left border-collapse min-w-[700px]">
              <thead>
                <tr className="border-b border-line text-[10px] text-muted uppercase tracking-[0.12em] font-extrabold bg-surface-2/20">
                  <th className="py-3 px-5">Snapshot Identifier</th>
                  <th className="py-3 px-5 w-40 text-center">Verification</th>
                  <th className="py-3 px-5 w-32">Data Size</th>
                  <th className="py-3 px-5 w-44">Created Time</th>
                  <th className="py-3 px-5 w-56 text-right">Actions</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line/40">
                {backups.map((b) => {
                  const getVerifyClass = (statusStr?: string) => {
                    if (statusStr === 'verified') return 'bg-good text-accent-on hover:bg-good';
                    if (statusStr === 'failed') return 'bg-bad text-accent-on hover:bg-bad';
                    return 'bg-warn text-accent-on hover:bg-warn';
                  };

                  return (
                    <tr key={b.id} className="hover:bg-surface-2/20 transition-colors group">
                      <td className="py-3.5 px-5 text-xs font-mono text-ink font-bold cursor-pointer select-all" onClick={() => { navigator.clipboard.writeText(b.id).then(() => { setCopied(b.id); setTimeout(() => setCopied(null), 800); }); }}>
                        {copied === b.id ? 'COPIED' : b.id}
                        {b.env_name && (
                          <span className="text-[10px] ml-2 px-1.5 py-0.5 rounded bg-surface-2 text-muted border border-line">
                            {b.env_name}
                          </span>
                        )}
                      </td>
                      <td className="py-2 px-5">
                        <div className={`mx-auto w-28 py-1 px-2 rounded-lg text-center font-extrabold text-[9px] uppercase tracking-wider shadow-[inset_0_-2px_0_rgba(0,0,0,0.15)] transition-colors select-none ${getVerifyClass(b.verification_status)}`}>
                          {b.verification_status || 'unverified'}
                        </div>
                      </td>
                      <td className="py-3.5 px-5 text-xs text-ink-soft font-semibold font-mono">
                        {formatBytes(b.size_bytes)}
                      </td>
                      <td className="py-3.5 px-5 text-xs text-muted font-mono">
                        {formatTime(b.timestamp)}
                      </td>
                      <td className="py-2 px-5 text-right flex items-center justify-end gap-2 h-[49px]">
                        <button 
                          onClick={() => verifyMutation.mutate(b.id)} 
                          disabled={verifyMutation.isPending} 
                          className="px-3 py-1.5 rounded-lg bg-surface-2 hover:bg-surface-3 border border-line text-[10px] font-bold uppercase tracking-wider text-ink-soft hover:text-ink disabled:opacity-30 transition-all flex items-center gap-1 shadow-sm active:translate-y-[0.5px]"
                        >
                          <ShieldCheck size={11} /> Verify
                        </button>
                        <button 
                          onClick={() => handleRestoreClick(b)}
                          disabled={restoreMutation.isPending} 
                          className="px-3 py-1.5 rounded-lg bg-warn/15 text-warn hover:bg-warn/25 border border-warn/20 text-[10px] font-bold uppercase tracking-wider transition-all disabled:opacity-30 flex items-center gap-1 active:translate-y-[0.5px]"
                        >
                          <RotateCcw size={11} /> Restore
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>
      </div>

      <RestoreConfirmModal
        open={!!restoreModal}
        backup={restoreModal?.backup ?? null}
        tables={dryRunTables}
        isLoading={dryRunLoading}
        confirmText={restoreConfirm}
        onConfirmTextChange={setRestoreConfirm}
        onConfirm={handleRestoreConfirm}
        onClose={() => { setRestoreModal(null); setRestoreConfirm(''); setDryRunTables([]); }}
      />
    </div>
  );
}
