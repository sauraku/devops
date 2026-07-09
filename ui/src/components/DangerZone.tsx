import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import * as api from '../lib/api';
import type { Project } from '../types';
import { Trash2, RotateCcw, StopCircle } from 'lucide-react';
import { useToast } from './Toast';

interface DangerZoneProps {
  project: Project;
}

export function DangerZone({ project }: DangerZoneProps) {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const [deleteConfirm, setDeleteConfirm] = useState('');
  const [rollbackCommit, setRollbackCommit] = useState('');

  const deleteMutation = useMutation({
    mutationFn: () => api.deleteProject(project.id, deleteConfirm),
    onSuccess: (data) => { 
      toast(data.message, data.ok ? 'success' : 'error'); 
      queryClient.invalidateQueries({ queryKey: ['projects'] }); 
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const rollbackMutation = useMutation({
    mutationFn: () => api.rollbackProject(project.id, rollbackCommit),
    onSuccess: (data) => {
      toast(data.operation?.id ? `Rollback started: ${data.operation.id}` : 'Rollback initiated', 'success');
      queryClient.invalidateQueries({ queryKey: ['project-status', project.id] });
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const abortMutation = useMutation({
    mutationFn: () => api.abortDeploy(project.id),
    onSuccess: (data) => { 
      toast(data.message, data.ok ? 'success' : 'warn'); 
      queryClient.invalidateQueries({ queryKey: ['project-status', project.id] }); 
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  return (
    <div className="max-w-3xl space-y-6">

      {/* Rollback Block */}
      <div className="glass-panel border border-warn/30 bg-gradient-to-br from-warn/10 to-transparent rounded-2xl p-6 shadow-lg">
        <h3 className="text-xs font-bold uppercase tracking-wider text-warn mb-1.5 flex items-center gap-2">
          <RotateCcw size={14} /> Rollback Deployment Release
        </h3>
        <p className="text-xs text-ink-soft mb-4 leading-relaxed">
          Rollback container services to a previous Git snapshot. Provide the release SHA hash. This builds and triggers a new run.
        </p>
        <div className="flex flex-col sm:flex-row gap-3">
          <input 
            value={rollbackCommit} 
            onChange={(e) => setRollbackCommit(e.target.value)} 
            placeholder="e.g. 5f4e3d2a1b0c9f8e7d6c5b4a" 
            className="flex-1 px-4 py-2.5 rounded-xl bg-surface-2/65 border border-line text-xs font-mono focus:outline-none focus:border-warn/40 focus:bg-surface-2 transition-all text-ink" 
          />
          <button 
            onClick={() => { 
              if (rollbackCommit.length < 7) return; 
              if (!window.confirm(`Initiate container rollback for "${project.id}" to SHA ${rollbackCommit.slice(0, 10)}?`)) return; 
              rollbackMutation.mutate(); 
            }} 
            disabled={rollbackMutation.isPending || rollbackCommit.length < 7} 
            className="px-5 py-2.5 rounded-xl bg-warn/15 hover:bg-warn/25 border border-warn/20 text-xs font-bold uppercase tracking-wider text-warn disabled:opacity-30 disabled:hover:bg-warn/10 transition-all flex items-center justify-center gap-1.5 shrink-0"
          >
            <RotateCcw size={13} /> {rollbackMutation.isPending ? 'ROLLING...' : 'ROLLBACK'}
          </button>
        </div>
      </div>

      {/* Abort Operations Block */}
      <div className="glass-panel border border-warn/30 bg-gradient-to-br from-warn/10 to-transparent rounded-2xl p-6 shadow-lg">
        <h3 className="text-xs font-bold uppercase tracking-wider text-warn mb-1.5 flex items-center gap-2">
          <StopCircle size={14} /> Force Interrupt Task
        </h3>
        <p className="text-xs text-ink-soft mb-4 leading-relaxed">
          Forcefully interrupt any currently running lock operations, deploy sequences, or backup configurations. Releasing locks might cause inconsistent states.
        </p>
        <button 
          onClick={() => { if (window.confirm('Interrupt the current active task pipeline?')) abortMutation.mutate(); }} 
          disabled={abortMutation.isPending} 
          className="px-5 py-2.5 rounded-xl bg-warn/15 hover:bg-warn/25 border border-warn/20 text-xs font-bold uppercase tracking-wider text-warn disabled:opacity-30 disabled:hover:bg-warn/10 transition-all flex items-center gap-1.5"
        >
          <StopCircle size={13} /> {abortMutation.isPending ? 'INTERRUPTING...' : 'FORCE INTERRUPT RUN'}
        </button>
      </div>

      {/* Delete Block */}
      <div className="glass-panel border border-bad/30 bg-gradient-to-br from-bad/10 to-transparent rounded-2xl p-6 shadow-lg glow-bad">
        <h3 className="text-xs font-bold uppercase tracking-wider text-bad mb-1.5 flex items-center gap-2">
          <Trash2 size={14} /> Terminate DevOps Project
        </h3>
        <p className="text-xs text-ink-soft mb-4 leading-relaxed">
          Permanently delete project metadata, configuration overrides, database backup listings, and remove runner listener configurations. Running container services will be stopped.
        </p>
        <div className="flex flex-col sm:flex-row gap-3">
          <input 
            value={deleteConfirm} 
            onChange={(e) => setDeleteConfirm(e.target.value)} 
            placeholder={`delete ${project.id}`} 
            className="flex-1 px-4 py-2.5 rounded-xl bg-surface-2/65 border border-line text-xs font-mono focus:outline-none focus:border-bad/40 focus:bg-surface-2 transition-all text-ink" 
          />
          <button 
            onClick={() => deleteMutation.mutate()} 
            disabled={deleteMutation.isPending || deleteConfirm !== `delete ${project.id}`} 
            className="px-5 py-2.5 rounded-xl bg-bad/15 hover:bg-bad/25 border border-bad/20 text-xs font-bold uppercase tracking-wider text-bad disabled:opacity-30 disabled:hover:bg-bad/10 transition-all flex items-center justify-center gap-1.5 shrink-0"
          >
            <Trash2 size={13} /> {deleteMutation.isPending ? 'TERMINATING...' : 'TERMINATE PROJECT'}
          </button>
        </div>
      </div>
    </div>
  );
}
