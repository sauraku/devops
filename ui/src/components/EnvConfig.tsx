import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useState, useEffect } from 'react';
import * as api from '../lib/api';
import type { Project } from '../types';
import { FileText, Check, ChevronDown, ChevronUp, Code, Key } from 'lucide-react';
import { useToast } from './Toast';
import {
  isMissingEnvValue,
  missingRequiredEnvVariables,
} from '../lib/env';

interface EnvConfigProps {
  project: Project;
}

export function EnvConfig({ project }: EnvConfigProps) {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const [values, setValues] = useState<Record<string, string>>({});
  const [showBulk, setShowBulk] = useState(false);
  const [bulkText, setBulkText] = useState('');
  const [ignoredBulkKeys, setIgnoredBulkKeys] = useState<string[]>([]);

  const { data, isLoading, error } = useQuery({
    queryKey: ['env-template', project.id],
    queryFn: () => api.getEnvTemplate(project.id),
  });

  const variables = data?.variables ?? [];
  const savedOverrides = data?.overrides ?? {};

  useEffect(() => {
    if (savedOverrides && Object.keys(savedOverrides).length > 0) {
      setValues((prev) => ({ ...savedOverrides, ...prev }));
    }
  }, [savedOverrides]);

  const missingRequiredVariables = missingRequiredEnvVariables(variables, values);
  const hasMissing = missingRequiredVariables.length > 0;

  const saveMutation = useMutation({
    mutationFn: () => api.saveEnvConfig(project.id, values),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['env-template', project.id] });
      toast('Environment variables saved successfully.', 'success');
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  if (isLoading) {
    return (
      <div className="flex flex-col items-center justify-center py-20 gap-3">
        <RefreshCw size={24} className="animate-spin text-accent" />
        <p className="text-xs text-muted font-bold uppercase tracking-wider">Parsing Env Templates...</p>
      </div>
    );
  }

  const emptyState = (
    <div className="glass-panel rounded-2xl p-8 max-w-xl mx-auto text-center border border-line shadow-2xl relative overflow-hidden">
      <div className="absolute top-0 right-0 w-24 h-24 bg-warn/10 rounded-full blur-2xl"></div>
      <div className="w-12 h-12 rounded-2xl bg-surface-2 border border-line-strong flex items-center justify-center mx-auto mb-4 text-muted">
        <FileText size={20} />
      </div>
      <h3 className="text-sm font-black uppercase tracking-wider text-ink mb-2">No Env Template Found</h3>
      {error ? (
        <p className="text-xs text-ink-soft leading-relaxed">
          {(error as Error)?.message?.includes('404') || (error as Error)?.message?.includes('not found')
            ? 'No .env.template template file exists inside this project workspace.'
            : `Failed to fetch template parameters: ${(error as Error).message}`}
        </p>
      ) : (
        <div className="space-y-3 text-xs text-ink-soft leading-relaxed">
          <p>DevOps Control was unable to find a <code className="text-accent bg-accent/10 px-1 rounded">.env.template</code> in the project root.</p>
          <p className="text-muted">
            For local repository projects, make sure <code className="text-accent font-mono">.env.template</code> is committed to the repository.
            For registry image projects, verify the template file is placed inside <code className="text-accent font-mono">{project.app_dir}</code>.
          </p>
        </div>
      )}
    </div>
  );

  if (variables.length === 0 && !isLoading) return emptyState;

  const missingCount = missingRequiredVariables.length;

  return (
    <div className="max-w-3xl space-y-6">

      {/* Main Variable Editor Panel */}
      <div className="glass-panel rounded-2xl border border-line overflow-hidden shadow-xl">
        <div className="px-6 py-5 bg-surface-2/20 border-b border-line flex flex-col sm:flex-row sm:items-center justify-between gap-4">
          <div>
            <h3 className="text-xs font-bold uppercase tracking-wider text-ink flex items-center gap-2">
              <FileText size={14} className="text-accent" />
              Environment Variables Config
            </h3>
            <p className="text-[10px] text-muted font-mono mt-1 select-all">
              Path: {project.app_dir || '.'}/.env.template
            </p>
          </div>
          {hasMissing && (
            <span className="text-[10px] font-bold uppercase tracking-wider px-2.5 py-1 rounded-full bg-bad/10 text-bad border border-bad/20 glow-bad select-none">
              {missingCount} variable{missingCount > 1 ? 's' : ''} missing
            </span>
          )}
        </div>

        <div className="p-6 space-y-5">
          {variables.map((v) => {
            const currentVal = values[v.key] ?? v.default ?? '';
            const isControllerManaged = v.controller_managed;
            const isMissing = v.operator_required && !isControllerManaged && isMissingEnvValue(currentVal);
            const isOptional = !v.operator_required && !isControllerManaged && isMissingEnvValue(currentVal);
            return (
              <div key={v.key} className="group relative">
                <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 mb-2">
                  <label className="text-xs font-extrabold font-mono text-ink tracking-tight flex items-center gap-1.5">
                    <Key size={11} className="text-muted" />
                    {v.key}
                  </label>
                  
                  {/* Monday status cell style badges */}
                  <div className="flex items-center gap-1.5 select-none">
                    {isControllerManaged && isMissingEnvValue(currentVal) ? (
                      <span className="text-[9px] font-bold uppercase tracking-wider px-2 py-0.5 rounded bg-accent/10 text-accent border border-accent/20">
                        Managed on deploy
                      </span>
                    ) : isMissing ? (
                      <span className="text-[9px] font-bold uppercase tracking-wider px-2 py-0.5 rounded bg-bad/10 text-bad border border-bad/20 glow-bad">
                        Missing
                      </span>
                    ) : isOptional ? (
                      <span className="text-[9px] font-bold uppercase tracking-wider px-2 py-0.5 rounded bg-surface-3 text-muted border border-line">
                        Optional
                      </span>
                    ) : (
                      <span className="text-[9px] font-bold uppercase tracking-wider px-2 py-0.5 rounded bg-good/10 text-good border border-good/20">
                        Configured
                      </span>
                    )}
                    {v.is_secret && (
                      <span className="text-[9px] font-bold uppercase tracking-wider px-2 py-0.5 rounded bg-purple-accent/10 text-purple-accent border border-purple-accent/20">
                        Secret Masked
                      </span>
                    )}
                  </div>
                </div>

                <input
                  type={v.is_secret ? 'password' : 'text'}
                  value={currentVal}
                  onChange={(e) => setValues((prev) => ({ ...prev, [v.key]: e.target.value }))}
                  placeholder={v.default || `Specify value for ${v.key}`}
                  className={`w-full px-4 py-2.5 rounded-xl bg-surface-2/65 border text-xs font-mono focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all text-ink ${
                    isMissing ? 'border-bad/30 bg-bad/2' : 'border-line'
                  }`}
                />
              </div>
            );
          })}

          {/* Bulk Paste Toggler */}
          <div className="border-t border-line/45 pt-4 mt-6">
            <button 
              onClick={() => setShowBulk(!showBulk)} 
              className="text-xs font-extrabold text-ink-soft hover:text-accent transition-colors flex items-center gap-1"
            >
              {showBulk ? <ChevronUp size={14} /> : <ChevronDown size={14} />}
              {showBulk ? 'Hide Bulk Imports' : 'Bulk Paste (KEY=VALUE)'}
            </button>
            
            {showBulk && (
              <div className="mt-4 space-y-2">
                <textarea
                  value={bulkText}
                  onChange={(e) => {
                    setBulkText(e.target.value);
                    const parsed: Record<string, string> = {};
                    const ignored = new Set<string>();
                    const declaredKeys = new Set(variables.map((variable) => variable.key));
                    const controllerManagedKeys = new Set(
                      variables.filter((variable) => variable.controller_managed).map((variable) => variable.key),
                    );
                    for (const line of e.target.value.split('\n')) {
                      const trimmed = line.trim();
                      if (!trimmed || trimmed.startsWith('#')) continue;
                      const eq = trimmed.indexOf('=');
                      if (eq <= 0) continue;
                      const key = trimmed.slice(0, eq).trim();
                      const value = trimmed.slice(eq + 1).trim();
                      if (!declaredKeys.has(key)) {
                        if (key) ignored.add(key);
                        continue;
                      }
                      // Do not turn an omitted controller-managed value into a
                      // blank override. Existing generated values stay sealed.
                      if (controllerManagedKeys.has(key) && value === '') continue;
                      parsed[key] = value;
                    }
                    setIgnoredBulkKeys([...ignored].sort());
                    setValues((prev) => ({ ...prev, ...parsed }));
                  }}
                  placeholder="PORT=8080&#10;DATABASE_URL=postgres://user:pass@host/db&#10;JWT_SECRET=super_secret"
                  className="w-full px-4 py-3 rounded-xl bg-surface-2/85 border border-line text-xs font-mono text-ink focus:outline-none focus:border-accent/40 focus:bg-surface-3 transition-colors"
                  rows={6}
                />
                <p className="text-[10px] text-muted">
                  Paste environment configuration directly, one line per variable. Local-only keys are ignored; blank controller-managed values are generated or derived during deployment.
                </p>
                {ignoredBulkKeys.length > 0 && (
                  <p className="text-[10px] text-warn">
                    Ignored {ignoredBulkKeys.length} key{ignoredBulkKeys.length === 1 ? '' : 's'} not declared by this project's template: {ignoredBulkKeys.join(', ')}
                  </p>
                )}
              </div>
            )}
          </div>

          <div className="flex items-center justify-between border-t border-line/45 pt-5 mt-6">
            <button
              onClick={() => saveMutation.mutate()}
              disabled={saveMutation.isPending}
              className="px-5 py-2.5 rounded-xl bg-gradient-to-r from-accent to-accent-hover hover:scale-[1.02] text-accent-on font-bold text-xs transition-all disabled:opacity-40 disabled:scale-100 flex items-center gap-1.5 shadow-[0_4px_15px_-3px_var(--accent)]/30"
            >
              <Check size={14} className="stroke-[3]" />
              {saveMutation.isPending ? 'SAVING...' : 'SAVE VARIABLES'}
            </button>
          </div>
        </div>
      </div>

      {/* Informational Guidelines Card */}
      <div className="glass-panel rounded-2xl p-5 border border-line flex gap-4">
        <div className="p-2 rounded-xl bg-accent/10 border border-accent/20 text-accent shrink-0 h-10 w-10 flex items-center justify-center">
          <Code size={18} />
        </div>
        <div>
          <h3 className="text-xs font-bold uppercase tracking-wider text-ink mb-1">Architecture Rules</h3>
          <ol className="text-xs text-ink-soft space-y-1.5 list-decimal list-inside leading-relaxed">
            <li>Variables defined in <code className="text-accent bg-surface-2 px-1 rounded">.env.template</code> serve as a template.</li>
            <li>Secrets are encrypted on the server-side and masked in UI displays.</li>
            <li>Controller-managed credentials are sealed automatically on the first deployment.</li>
            <li>Saved configurations are merged and injected as container environments during runs.</li>
          </ol>
        </div>
      </div>
    </div>
  );
}

// Stub/helper to prevent missing imports
import { RefreshCw } from 'lucide-react';
