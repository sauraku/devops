import { useState, useEffect } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import type { Project } from '../types';
import * as api from '../lib/api';
import { X, Box, AlertTriangle, Info, ShieldCheck, Settings, Key } from 'lucide-react';

interface ProjectModalProps {
  open: boolean;
  onClose: () => void;
  project?: Project | null;
}

const emptyForm = {
  id: '',
  name: '',
  repo_url: 'https://github.com/username/repo',
  branch_name: 'dev',
  runner_token: '',
  deployment_mode: 'compose_image',
  auto_apply: 'true',
  registry_host: 'ghcr.io',
  registry_username: '',
  registry_password: '',
  app_dir: '',
};

type ModalTab = 'general' | 'auth';

export function ProjectModal({ open, onClose, project }: ProjectModalProps) {
  const queryClient = useQueryClient();
  const [form, setForm] = useState(emptyForm);
  const [errors, setErrors] = useState<string[]>([]);
  const [activeTab, setActiveTab] = useState<ModalTab>('general');

  const appDirPlaceholder = form.id ? `Projects/${form.id}` : 'e.g. /data/projects/my-project';

  useEffect(() => {
    if (project) {
      setForm({
        id: project.id,
        name: project.name ?? '',
        repo_url: project.repo_url ?? '',
        branch_name: project.branch_name ?? 'rc',
        runner_token: '',
        deployment_mode: project.deployment_mode ?? 'compose_image',
        auto_apply: String(project.auto_apply ?? true),
        registry_host: project.registry_host ?? 'ghcr.io',
        registry_username: project.registry_username ?? '',
        registry_password: '',
        app_dir: project.app_dir ?? '',
      });
    } else {
      setForm(emptyForm);
    }
    setErrors([]);
  }, [project, open]);

  const mutation = useMutation({
    mutationFn: () => {
      const body: Record<string, any> = {};
      if (!project) body.id = form.id;
      body.name = form.name || form.id || undefined;
      body.repo_url = form.repo_url || undefined;
      body.branch_name = form.branch_name;
      body.runner_token = form.runner_token || undefined;
      body.deployment_mode = form.deployment_mode;
      body.auto_apply = form.auto_apply === 'true';
      body.registry_host = form.registry_host || undefined;
      body.registry_username = form.registry_username || undefined;
      body.registry_password = form.registry_password || undefined;
      body.listener_active = !!form.runner_token;
      body.app_dir = form.app_dir || undefined;

      return project
        ? api.updateProjectConfig(project.id, body)
        : api.createProject(body);
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['projects'] });
      queryClient.invalidateQueries({ queryKey: ['project-status'] });
      onClose();
    },
    onError: (err: Error) => {
      setErrors([err.message]);
    },
  });

  if (!open) return null;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-bg/80 backdrop-blur-md transition-all p-4" onClick={onClose}>
      {/* Modal Card */}
      <div
        className="glass-panel border border-line-strong rounded-3xl w-full max-w-xl max-h-[90vh] overflow-hidden shadow-2xl flex flex-col relative"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="absolute top-0 right-0 w-36 h-36 bg-purple-accent/5 rounded-full blur-3xl pointer-events-none"></div>

        {/* Modal Header */}
        <div className="px-6 py-5 border-b border-line flex items-center justify-between">
          <div className="flex items-center gap-2.5">
            <div className="p-1.5 rounded-xl bg-accent/15 border border-accent/25 text-accent">
              <Box size={16} />
            </div>
            <h2 className="text-sm font-black uppercase tracking-wider text-ink">
              {project ? 'Edit DevOps Configuration' : 'Register New Project'}
            </h2>
          </div>
          <button 
            onClick={onClose} 
            className="text-muted hover:text-ink p-1.5 rounded-xl hover:bg-surface-2/60 transition-colors"
          >
            <X size={16} />
          </button>
        </div>

        {/* Inner Modal Tabs Selector */}
        <div className="px-6 bg-surface-2/20 border-b border-line flex gap-2 pt-2 select-none">
          <button
            onClick={() => setActiveTab('general')}
            className={`flex items-center gap-1.5 px-3 py-2 text-xs font-bold transition-all border-b-2 ${
              activeTab === 'general'
                ? 'text-accent border-accent'
                : 'text-ink-soft border-transparent hover:text-ink'
            }`}
          >
            <Settings size={12} />
            <span>General Setup</span>
          </button>
          <button
            onClick={() => setActiveTab('auth')}
            className={`flex items-center gap-1.5 px-3 py-2 text-xs font-bold transition-all border-b-2 ${
              activeTab === 'auth'
                ? 'text-accent border-accent'
                : 'text-ink-soft border-transparent hover:text-ink'
            }`}
          >
            <Key size={12} />
            <span>Registry Credentials</span>
          </button>
        </div>

        {/* Modal Body (Scrollable) */}
        <div className="p-6 overflow-y-auto flex-1 space-y-4">
          {errors.length > 0 && (
            <div className="p-4 rounded-2xl bg-bad/10 border border-bad/20 text-xs text-bad flex items-start gap-2.5 glow-bad">
              <AlertTriangle size={15} className="shrink-0 mt-0.5" />
              <div>{errors.map((e, i) => <p key={i} className="font-bold">{e}</p>)}</div>
            </div>
          )}

          <form 
            onSubmit={(e) => { e.preventDefault(); setErrors([]); mutation.mutate(); }} 
            className="space-y-4"
          >
            {activeTab === 'general' && (
              <div className="space-y-4">
                {!project && (
                  <div>
                    <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">
                      Project ID <span className="text-bad">*</span>
                    </label>
                    <input 
                      value={form.id} 
                      onChange={(e) => setForm({ ...form, id: e.target.value })} 
                      required 
                      placeholder="e.g. my-project" 
                      className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all font-mono" 
                    />
                  </div>
                )}
                
                <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
                  <div>
                    <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">
                      Display Name <span className="text-muted font-medium normal-case tracking-normal">(default: same as ID)</span>
                    </label>
                    <input 
                      value={form.name} 
                      onChange={(e) => setForm({ ...form, name: e.target.value })} 
                      placeholder="e.g. My API" 
                      className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all" 
                    />
                  </div>
                  <div>
                    <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">
                      Workspace Directory <span className="text-muted font-medium normal-case tracking-normal">(default: ~/Projects/{'{id}'})</span>
                    </label>
                    <input 
                      value={form.app_dir} 
                      onChange={(e) => setForm({ ...form, app_dir: e.target.value })} 
                      placeholder={appDirPlaceholder}
                      className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all font-mono" 
                    />
                  </div>
                </div>

                <div>
                    <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">
                      Repository URL <span className="text-muted font-medium normal-case tracking-normal">(default: github.com/sauraku/{'{id}'})</span>
                    </label>
                  <input 
                    value={form.repo_url} 
                    onChange={(e) => setForm({ ...form, repo_url: e.target.value })} 
                    placeholder="https://github.com/your-org/your-project" 
                    className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all font-mono" 
                  />
                </div>

                <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
                  <div>
                    <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">
                      Target Release Branch <span className="text-muted font-medium normal-case tracking-normal">(default: dev)</span>
                    </label>
                    <input 
                      value={form.branch_name} 
                      onChange={(e) => setForm({ ...form, branch_name: e.target.value })} 
                      placeholder="main" 
                      className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all font-mono" 
                    />
                  </div>
                  <div>
                    <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">GitHub Runner Token</label>
                    <input 
                      type="password" 
                      value={form.runner_token} 
                      onChange={(e) => setForm({ ...form, runner_token: e.target.value })} 
                      placeholder={project ? '•••••••••••• (Leave blank to keep)' : 'Paste GitHub PAT/runner token'} 
                      className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all" 
                    />
                  </div>
                </div>

                <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
                  <div>
                    <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Deployment Mode</label>
                    <select 
                      value={form.deployment_mode} 
                      onChange={(e) => setForm({ ...form, deployment_mode: e.target.value })} 
                      className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all"
                    >
                      <option value="compose_image">Docker Registry Deploy</option>
                      <option value="local_repo">Local Git Checkouts</option>
                    </select>
                  </div>
                  <div>
                    <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Continuous Delivery Mode</label>
                    <select 
                      value={form.auto_apply} 
                      onChange={(e) => setForm({ ...form, auto_apply: e.target.value })} 
                      className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all"
                    >
                      <option value="true">Auto-Apply on push Webhook</option>
                      <option value="false">Manual Approval Required</option>
                    </select>
                  </div>
                </div>
              </div>
            )}

            {activeTab === 'auth' && (
              <div className="space-y-4">
                <div className="p-4 rounded-2xl bg-surface-2/40 border border-line flex gap-3 text-xs text-ink-soft leading-relaxed">
                  <Info size={16} className="text-accent shrink-0 mt-0.5" />
                  <p>Credentials provided below are used by the DevOps control plane runner to authenticate and pull docker release images from secure container repositories.</p>
                </div>

                <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
                  <div>
                    <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Registry Host Endpoint</label>
                    <input 
                      value={form.registry_host} 
                      onChange={(e) => setForm({ ...form, registry_host: e.target.value })} 
                      placeholder="ghcr.io" 
                      className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all" 
                    />
                  </div>
                  <div>
                    <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Registry Username</label>
                    <input 
                      value={form.registry_username} 
                      onChange={(e) => setForm({ ...form, registry_username: e.target.value })} 
                      placeholder="e.g. github-username" 
                      className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all" 
                    />
                  </div>
                </div>

                <div>
                  <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">PAT / Access Token Secrets</label>
                  <input 
                    type="password" 
                    value={form.registry_password} 
                    onChange={(e) => setForm({ ...form, registry_password: e.target.value })} 
                    placeholder={project?.registry_username ? '•••••••••••• (Leave blank to keep)' : 'Paste Personal Access Token'} 
                    className="w-full px-3.5 py-2.5 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all" 
                  />
                </div>
              </div>
            )}

            {/* Modal Footer Controls */}
            <div className="border-t border-line pt-4 flex gap-3.5 justify-end">
              <button 
                type="button" 
                onClick={onClose} 
                className="px-5 py-2.5 rounded-xl bg-surface-2 hover:bg-surface-3 border border-line text-xs font-bold text-ink-soft hover:text-ink transition-colors"
              >
                Cancel
              </button>
              <button 
                type="submit" 
                disabled={mutation.isPending} 
                className="px-6 py-2.5 rounded-xl bg-gradient-to-r from-accent to-accent-hover hover:scale-[1.02] text-accent-on font-bold text-xs transition-all disabled:opacity-40 disabled:scale-100 flex items-center gap-1.5 shadow-[0_4px_15px_-3px_var(--accent)]/30"
              >
                <ShieldCheck size={14} className="stroke-[3]" />
                {mutation.isPending ? 'SAVING...' : 'SAVE CONFIG'}
              </button>
            </div>
          </form>
        </div>
      </div>
    </div>
  );
}
