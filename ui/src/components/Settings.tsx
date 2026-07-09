import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import * as api from '../lib/api';
import type { Project } from '../types';
import { Play, Square, RotateCw, Copy, Check, Terminal, Shield, Cpu, Code } from 'lucide-react';
import { useToast } from './Toast';

interface SettingsProps {
  project: Project;
}

export function Settings({ project }: SettingsProps) {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const [repoUrl, setRepoUrl] = useState(project.repo_url);
  const [branchName, setBranchName] = useState(project.branch_name);
  const [runnerToken, setRunnerToken] = useState('');
  const [listenerActive, setListenerActive] = useState(project.runner_status === 'active');
  const [regHost, setRegHost] = useState(project.registry_host);
  const [regUser, setRegUser] = useState(project.registry_username);
  const [regPass, setRegPass] = useState('');
  const [autoApply, setAutoApply] = useState(project.auto_apply);
  const [appDir, setAppDir] = useState(project.app_dir);
  const [copiedField, setCopiedField] = useState<string | null>(null);

  const webhookUrl = `${window.location.origin}/api/projects/${encodeURIComponent(project.id)}/deploy`;
  const runnerLabel = `self-hosted,linux,x64,project-${project.id},branch-${project.branch_name}`;

  const workflowYaml = `name: Deploy
on:
  push:
    branches: [ ${project.branch_name} ]

jobs:
  deploy:
    runs-on: [self-hosted, linux, x64, project-${project.id}]
    steps:
      - name: Trigger Deploy
        run: |
          curl -X POST \\
            -H "Authorization: Bearer \${{ secrets.DEPLOY_CONTROL_TOKEN }}" \\
            -H "Content-Type: application/json" \\
            -d '{"ref": "\${{ github.ref }}", "sha": "\${{ github.sha }}", "branch": "${project.branch_name}", "commit_message": "\${{ github.event.head_commit.message }}"}' \\
            \${{ secrets.DEPLOY_CONTROL_URL }}/api/projects/${project.id}/deploy`;

  const mutation = useMutation({
    mutationFn: () =>
      api.updateProjectConfig(project.id, {
        repo_url: repoUrl || undefined,
        branch_name: branchName,
        runner_token: runnerToken || undefined,
        listener_active: listenerActive,
        registry_host: regHost || undefined,
        registry_username: regUser || undefined,
        registry_password: regPass || undefined,
        app_dir: appDir || undefined,
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['project-status', project.id] });
      queryClient.invalidateQueries({ queryKey: ['projects'] });
      setRunnerToken('');
      setRegPass('');
      toast('Project configurations updated successfully.', 'success');
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const runnerStartMutation = useMutation({
    mutationFn: () => api.runnerAction(project.id, 'start'),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['project-status', project.id] });
      toast('GitHub Actions Runner service active.', 'success');
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const runnerStopMutation = useMutation({
    mutationFn: () => api.runnerAction(project.id, 'stop'),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['project-status', project.id] });
      toast('GitHub Actions Runner service offline.', 'success');
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const runnerRestartMutation = useMutation({
    mutationFn: () => api.runnerAction(project.id, 'restart'),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['project-status', project.id] });
      toast('GitHub Actions Runner restarted.', 'success');
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const copyToClipboard = (text: string, fieldId: string) => {
    navigator.clipboard.writeText(text);
    setCopiedField(fieldId);
    setTimeout(() => setCopiedField(null), 2000);
  };

  return (
    <div className="max-w-3xl space-y-6">

      {/* Runner Service Manager Widget */}
      <div className="glass-panel border border-line rounded-2xl p-6 shadow-xl relative overflow-hidden">
        <div className="absolute top-0 right-0 w-32 h-32 bg-accent/5 rounded-full blur-2xl"></div>
        <div className="flex items-center gap-2.5 mb-2">
          <Cpu size={14} className="text-accent" />
          <h3 className="text-xs font-bold uppercase tracking-wider text-ink">GitHub Self-Hosted Runner</h3>
        </div>
        <p className="text-xs text-ink-soft mb-5 leading-relaxed">
          Manage local deployment daemon services connected directly to GitHub repositories. Active runners fetch workflows automatically.
        </p>
        
        <div className="flex flex-wrap gap-2.5">
          <button 
            onClick={() => runnerStartMutation.mutate()} 
            disabled={runnerStartMutation.isPending} 
            className="px-4 py-2 rounded-xl bg-good/10 text-good border border-good/20 text-xs font-extrabold hover:bg-good/15 transition-all disabled:opacity-30 flex items-center gap-1.5 shadow-sm active:translate-y-[0.5px]"
          >
            <Play size={12} className="fill-current" /> Start Daemon
          </button>
          <button 
            onClick={() => runnerStopMutation.mutate()} 
            disabled={runnerStopMutation.isPending} 
            className="px-4 py-2 rounded-xl bg-bad/10 text-bad border border-bad/20 text-xs font-extrabold hover:bg-bad/15 transition-all disabled:opacity-30 flex items-center gap-1.5 shadow-sm glow-bad active:translate-y-[0.5px]"
          >
            <Square size={12} className="fill-current" /> Stop Daemon
          </button>
          <button 
            onClick={() => runnerRestartMutation.mutate()} 
            disabled={runnerRestartMutation.isPending} 
            className="px-4 py-2 rounded-xl bg-warn/10 text-warn border border-warn/20 text-xs font-extrabold hover:bg-warn/15 transition-all disabled:opacity-30 flex items-center gap-1.5 shadow-sm active:translate-y-[0.5px]"
          >
            <RotateCw size={12} /> Restart
          </button>
        </div>
      </div>

      {/* Main Configurations Form */}
      <div className="glass-panel border border-line rounded-2xl p-6 shadow-xl">
        <div className="flex items-center gap-2.5 mb-5 border-b border-line/45 pb-4">
          <Terminal size={14} className="text-accent" />
          <h3 className="text-xs font-bold uppercase tracking-wider text-ink">Project Coordinates</h3>
        </div>

        <form onSubmit={(e) => { e.preventDefault(); mutation.mutate(); }} className="space-y-5">
          <div>
            <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Repository URL</label>
            <input 
              value={repoUrl} 
              onChange={(e) => setRepoUrl(e.target.value)} 
              className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all font-mono" 
            />
          </div>

          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div>
              <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Target Git Branch</label>
              <input 
                value={branchName} 
                onChange={(e) => setBranchName(e.target.value)} 
                className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all font-mono" 
              />
            </div>
            <div>
              <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Continuous Delivery Mode</label>
              <select 
                value={autoApply ? 'true' : 'false'} 
                onChange={(e) => setAutoApply(e.target.value === 'true')} 
                className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all"
              >
                <option value="true">Auto-Apply on Push Webhook</option>
                <option value="false">Manual Execution Required</option>
              </select>
            </div>
          </div>

          <div>
            <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">App Workspace Directory</label>
            <input 
              value={appDir} 
              onChange={(e) => setAppDir(e.target.value)} 
              placeholder="/var/www/my-project" 
              className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs font-mono focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all" 
            />
            <p className="text-[10px] text-muted mt-1.5">Path coordinates holding project configuration files like compose, env, and script targets.</p>
          </div>

          <div>
            <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Runner Token Authentication (Write-Only)</label>
            <input 
              type="password" 
              value={runnerToken} 
              onChange={(e) => setRunnerToken(e.target.value)} 
              placeholder="••••••••••••••••••••••••••••••••" 
              className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all" 
            />
          </div>

          <label className="flex items-center gap-2.5 text-xs text-ink-soft cursor-pointer select-none">
            <input 
              type="checkbox" 
              checked={listenerActive} 
              onChange={(e) => setListenerActive(e.target.checked)} 
              className="rounded bg-surface-2 border-line text-accent accent-accent w-4 h-4" 
            />
            <span>Auto-Start Listener (spawn daemon at startup)</span>
          </label>

          <div className="border-t border-line/45 pt-5 mt-5">
            <div className="flex items-center gap-2.5 mb-4">
              <Shield size={14} className="text-accent" />
              <h4 className="text-xs font-bold uppercase tracking-wider text-ink">Container Registry Authentication</h4>
            </div>

            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div>
                <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Registry Endpoint URL</label>
                <input 
                  value={regHost} 
                  onChange={(e) => setRegHost(e.target.value)} 
                  placeholder="ghcr.io" 
                  className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all" 
                />
              </div>
              <div>
                <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Username Credentials</label>
                <input 
                  value={regUser} 
                  onChange={(e) => setRegUser(e.target.value)} 
                  placeholder="e.g. github-username" 
                  className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all" 
                />
              </div>
            </div>

            <div className="mt-4">
              <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Password / Personal Access Token (Write-Only)</label>
              <input 
                type="password" 
                value={regPass} 
                onChange={(e) => setRegPass(e.target.value)} 
                placeholder={regUser ? '••••••••••••••••••••••••••••••••' : 'Enter personal access token'} 
                className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all" 
              />
            </div>
          </div>

          <div className="border-t border-line/45 pt-5 mt-5">
            <button 
              type="submit" 
              disabled={mutation.isPending} 
              className="px-5 py-2.5 rounded-xl bg-gradient-to-r from-accent to-accent-hover hover:scale-[1.02] text-accent-on font-bold text-xs transition-all disabled:opacity-40 disabled:scale-100 flex items-center justify-center gap-1.5 shadow-[0_4px_15px_-3px_var(--accent)]/30"
            >
              <Check size={14} className="stroke-[3]" />
              {mutation.isPending ? 'SAVING...' : 'SAVE CONFIGURATION'}
            </button>
          </div>
        </form>
      </div>

      {/* CI/CD Webhook & Scripts Payload */}
      <div className="glass-panel border border-line rounded-2xl p-6 shadow-xl">
        <div className="flex items-center gap-2.5 mb-5">
          <Code size={14} className="text-accent" />
          <h3 className="text-xs font-bold uppercase tracking-wider text-ink">CI/CD Automation payload snippets</h3>
        </div>

        <div className="space-y-5">
          <div>
            <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Incoming Trigger Webhook URL</label>
            <div className="flex gap-2">
              <input 
                readOnly 
                value={webhookUrl} 
                className="flex-1 px-3.5 py-2.5 rounded-xl bg-surface-2 border border-line text-ink text-xs font-mono focus:outline-none" 
              />
              <button 
                onClick={() => copyToClipboard(webhookUrl, 'webhook')} 
                className="px-4 py-2.5 rounded-xl bg-surface-3 hover:bg-surface-2 border border-line text-xs font-bold text-ink-soft hover:text-ink transition-all flex items-center gap-1.5 shrink-0"
              >
                {copiedField === 'webhook' ? <Check size={13} className="text-good" /> : <Copy size={13} />}
                <span>{copiedField === 'webhook' ? 'Copied' : 'Copy'}</span>
              </button>
            </div>
          </div>

          <div>
            <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Runner Target Labels</label>
            <div className="flex gap-2">
              <input 
                readOnly 
                value={runnerLabel} 
                className="flex-1 px-3.5 py-2.5 rounded-xl bg-surface-2 border border-line text-ink text-xs font-mono focus:outline-none" 
              />
              <button 
                onClick={() => copyToClipboard(runnerLabel, 'labels')} 
                className="px-4 py-2.5 rounded-xl bg-surface-3 hover:bg-surface-2 border border-line text-xs font-bold text-ink-soft hover:text-ink transition-all flex items-center gap-1.5 shrink-0"
              >
                {copiedField === 'labels' ? <Check size={13} className="text-good" /> : <Copy size={13} />}
                <span>{copiedField === 'labels' ? 'Copied' : 'Copy'}</span>
              </button>
            </div>
          </div>

          <div>
            <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Example GitHub Workflow configuration</label>
            <div className="flex gap-2.5 items-start">
              <pre className="flex-1 p-4 rounded-xl bg-bg border border-line text-ink text-xs font-mono overflow-x-auto max-h-56 select-all leading-relaxed">
                {workflowYaml}
              </pre>
              <button 
                onClick={() => copyToClipboard(workflowYaml, 'yaml')} 
                className="px-4 py-2.5 rounded-xl bg-surface-3 hover:bg-surface-2 border border-line text-xs font-bold text-ink-soft hover:text-ink transition-all flex items-center gap-1.5 shrink-0 self-start shadow-sm"
              >
                {copiedField === 'yaml' ? <Check size={13} className="text-good" /> : <Copy size={13} />}
                <span>{copiedField === 'yaml' ? 'Copied' : 'Copy'}</span>
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
