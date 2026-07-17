import { lazy, Suspense, useState, useCallback, useEffect } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Sidebar } from '../components/Sidebar';
import { Dashboard } from '../components/Dashboard';
import { Settings } from '../components/Settings';
import { DangerZone } from '../components/DangerZone';
import { Backups } from '../components/Backups';
import { LogViewer } from '../components/LogViewer';
import { EnvConfig } from '../components/EnvConfig';
import { ProjectModal } from '../components/ProjectModal';
import { AestheticsCustomizer } from '../components/AestheticsCustomizer';
import * as api from '../lib/api';
import { missingRequiredEnvVariables } from '../lib/env';
import type { Project } from '../types';
import { LayoutDashboard, Container, FileText, Archive, Settings2, Flame, AlertTriangle, Box, Plus, RefreshCw, Terminal as TerminalIcon } from 'lucide-react';

const Terminal = lazy(() =>
  import('../components/Terminal').then((module) => ({ default: module.Terminal })),
);

type Tab = 'dashboard' | 'containers' | 'env' | 'backups' | 'settings' | 'terminal' | 'danger';

const tabDefs: { id: Tab; label: string; icon: React.ComponentType<{ size?: number }> }[] = [
  { id: 'dashboard', label: 'Dashboard', icon: LayoutDashboard },
  { id: 'containers', label: 'Containers & Logs', icon: Container },
  { id: 'env', label: 'Environment', icon: FileText },
  { id: 'backups', label: 'Backups', icon: Archive },
  { id: 'settings', label: 'Settings', icon: Settings2 },
  { id: 'danger', label: 'Danger Zone', icon: Flame },
];

const terminalTab = { id: 'terminal' as const, label: 'Terminal', icon: TerminalIcon };

function tabFromHash(): Tab {
  const hash = window.location.hash.replace('#', '') as Tab;
  return [...tabDefs, terminalTab].map(t => t.id).includes(hash) ? hash : 'dashboard';
}

export function ProjectPage() {
  const [selectedId, setSelectedId] = useState<string>('');
  const [showModal, setShowModal] = useState(false);
  const [editingProject, setEditingProject] = useState<Project | null>(null);
  const [activeTab, setActiveTab] = useState<Tab>(tabFromHash);
  const [deployId, setDeployId] = useState<string>('');
  const [theme, setTheme] = useState(() => localStorage.getItem('theme') || 'default');

  const handleThemeChange = (newTheme: string) => {
    setTheme(newTheme);
    localStorage.setItem('theme', newTheme);
    localStorage.setItem('userPickedTheme', 'true');
    document.documentElement.setAttribute('data-theme', newTheme);
  };

  const { data: projectsData, isLoading: projectsLoading, error: projectsError } = useQuery({
    queryKey: ['projects'],
    queryFn: api.listProjects,
    refetchInterval: 15000,
  });

  const projects = projectsData?.projects ?? [];
  const defaultId = projectsData?.default_project_id ?? '';

  useEffect(() => {
    const effectiveId = selectedId || defaultId || projects[0]?.id || '';
    if (effectiveId && effectiveId !== selectedId) {
      setSelectedId(effectiveId);
    }
  }, [selectedId, defaultId, projects]);

  useEffect(() => {
    const onHashChange = () => setActiveTab(tabFromHash());
    window.addEventListener('hashchange', onHashChange);
    return () => window.removeEventListener('hashchange', onHashChange);
  }, []);

  const selectedProject = projects.find((p) => p.id === selectedId) ?? null;

  const { data: healthStatus } = useQuery({
    queryKey: ['project-status', selectedId],
    queryFn: () => api.getProjectStatus(selectedId),
    refetchInterval: 15000,
    enabled: !!selectedId,
  });

  const containerHealth = healthStatus?.containers?.current ?? {};
	const availableTabs = healthStatus?.capabilities?.terminal
		? [...tabDefs.slice(0, -1), terminalTab, tabDefs[tabDefs.length - 1]]
		: tabDefs;

	useEffect(() => {
		if (activeTab === 'terminal' && healthStatus && !healthStatus.capabilities?.terminal) {
			setActiveTab('dashboard');
			window.location.hash = 'dashboard';
		}
	}, [activeTab, healthStatus]);
  const totalContainers = Object.keys(containerHealth).length;
  const unhealthyCount = Object.entries(containerHealth).filter(([_, s]) => s !== 'running').length;

  const { data: envTemplateData } = useQuery({
    queryKey: ['env-template', selectedId],
    queryFn: () => api.getEnvTemplate(selectedId),
    staleTime: 30000,
    enabled: !!selectedId,
  });
  const envVars = envTemplateData?.variables ?? [];
  const envOverrides = envTemplateData?.overrides ?? {};
  const missingEnvVars = missingRequiredEnvVariables(envVars, envOverrides);
  const envMissingCount = envVars.length > 0 ? missingEnvVars.length : 0;
  const healthStatusIndicator = totalContainers === 0 ? 'neutral'
    : unhealthyCount === 0 ? 'good'
    : unhealthyCount === totalContainers ? 'bad'
    : 'warn';

  const handleSelectProject = useCallback((id: string) => {
    setSelectedId(id);
    setActiveTab('dashboard');
    setDeployId('');
    window.location.hash = 'dashboard';
  }, []);

  const handleTabChange = useCallback((tab: Tab) => {
    setActiveTab(tab);
    window.location.hash = tab;
  }, []);

  return (
    <div className="flex flex-col md:flex-row h-full w-full overflow-hidden">
      {/* Sidebar - Horizontal header on Mobile, Vertical drawer on Desktop */}
      <Sidebar
        projects={projects}
        selectedId={selectedId}
        onSelect={handleSelectProject}
        onAddProject={() => { setEditingProject(null); setShowModal(true); }}
        theme={theme}
        onThemeChange={handleThemeChange}
      />

      <main className="flex-1 flex flex-col min-h-0 w-full">
        {selectedProject ? (
          <>
            {/* Industrial Header */}
            <header className="px-4 pt-4 sm:px-6 sm:pt-5 bg-surface border-b border-line">
              {/* Breadcrumbs */}
              <div className="flex items-center gap-1.5 text-[10px] text-muted font-mono select-none uppercase tracking-wider mb-2.5">
                <span>ROOT</span>
                <span>/</span>
                <span>WORKSPACE</span>
                <span>/</span>
                <span className="text-accent font-bold flex items-center gap-1.5">
                  {selectedProject.name || selectedProject.id}
                  <span className={`w-2 h-2 rounded-full ${
                    healthStatusIndicator === 'good' ? 'bg-good' :
                    healthStatusIndicator === 'bad' ? 'bg-bad animate-pulse' :
                    healthStatusIndicator === 'warn' ? 'bg-warn' :
                    'bg-muted'
                  }`} title={`${unhealthyCount}/${totalContainers} unhealthy`} />
                </span>
              </div>

              {/* Title & Actions Row */}
              <div className="flex flex-col md:flex-row md:items-center justify-between gap-4 pb-4">
                <div>
                  <div className="flex items-center gap-3">
                    <h1 className="text-xl font-black tracking-tight text-ink uppercase">
                      {selectedProject.name || selectedProject.id}
                    </h1>

                    {/* Status Badge */}
                    <div className={`px-2.5 py-1 rounded-full text-xs font-semibold flex items-center gap-1.5 border select-none ${
                      selectedProject.runner_status === 'active'
                        ? 'bg-good/10 text-good border-good/20 glow-good'
                        : selectedProject.runner_status === 'starting'
                        ? 'bg-warn/10 text-warn border-warn/20'
                        : selectedProject.runner_status === 'error'
                        ? 'bg-bad/10 text-bad border-bad/20'
                        : 'bg-warn/10 text-warn border-warn/20'
                    }`}>
                      <span className={`w-2 h-2 rounded-full ${
                        selectedProject.runner_status === 'active' ? 'bg-good animate-pulse' :
                        selectedProject.runner_status === 'starting' ? 'bg-warn animate-pulse' :
                        selectedProject.runner_status === 'error' ? 'bg-bad' : 'bg-warn'
                      }`}></span>
                      {selectedProject.runner_status?.toUpperCase() || 'UNKNOWN'}
                    </div>
                  </div>
                  <p className="text-[10px] text-muted font-mono mt-1.5 select-all uppercase">
                    REPOSITORY: {selectedProject.repo_url}
                  </p>
                </div>

                <div className="flex items-center gap-2">
                  <button
                    onClick={() => {
                      setEditingProject(selectedProject);
                      setShowModal(true);
                    }}
                    className="px-3.5 py-1.5 rounded bg-surface-2 border border-line text-[10px] font-bold uppercase tracking-wider text-ink-soft hover:text-ink hover:bg-surface-3 transition-colors active:translate-y-[0.5px]"
                  >
                    CONFIG
                  </button>
                </div>
              </div>

              {/* Industrial Card-Index Tab Bar */}
              <div role="tablist" className="flex items-end overflow-x-auto pt-2">
                {availableTabs.map((tab) => {
                  const Icon = tab.icon;
                  const active = activeTab === tab.id;
                  return (
                    <button
                      key={tab.id}
                      role="tab"
                      aria-selected={active}
                      title={tab.label}
                      aria-label={tab.label}
                      onClick={() => handleTabChange(tab.id)}
                      className={`flex items-center gap-2 px-3 sm:px-5 py-2.5 text-[10px] font-bold tracking-widest uppercase select-none transition-all border-r border-line shrink-0 relative ${
                        active
                          ? 'text-accent bg-bg border-t-2 border-t-accent border-l border-r border-line border-b-transparent'
                          : 'text-muted bg-surface-2/40 hover:text-ink hover:bg-surface-2/80 border-t border-t-transparent border-b border-b-line'
                      }`}
                    >
                      <span className={active ? 'text-accent' : 'text-muted'}>
                        <Icon size={12} />
                      </span>
                      <span className="hidden sm:inline">{tab.label}</span>
                      {tab.id === 'env' && envMissingCount > 0 && (
                        <span className="ml-1 px-1.5 py-0.5 rounded-full bg-bad text-accent-on text-[8px] font-bold">{envMissingCount}</span>
                      )}
                      {tab.id === 'containers' && unhealthyCount > 0 && (
                        <span className="ml-1 px-1.5 py-0.5 rounded-full bg-warn text-accent-on text-[8px] font-bold">{unhealthyCount}</span>
                      )}
                    </button>
                  );
                })}
                <div className="flex-1 border-b border-line h-[35px]"></div>
              </div>
            </header>

            {/* Content Viewport */}
            <div className={`flex-1 min-h-0 bg-bg relative ${
              (activeTab === 'containers' || activeTab === 'terminal') ? 'flex flex-col overflow-hidden p-4 sm:p-6' : 'overflow-auto p-4 sm:p-6'
            }`}>
              <div className={(activeTab === 'containers' || activeTab === 'terminal') ? 'flex-1 flex flex-col min-h-0 w-full' : 'max-w-7xl mx-auto'}>
                {activeTab === 'dashboard' && <Dashboard project={selectedProject} />}
                {activeTab === 'containers' && (
                  <LogViewer project={selectedProject} deployId={deployId} onDeploySelect={setDeployId} />
                )}
                {activeTab === 'env' && <EnvConfig project={selectedProject} />}
                {activeTab === 'backups' && <Backups project={selectedProject} />}
                {activeTab === 'settings' && <Settings project={selectedProject} />}
                {activeTab === 'terminal' && healthStatus?.capabilities?.terminal && (
                  <Suspense fallback={<div className="text-xs font-mono text-muted">Loading terminal…</div>}>
                    <Terminal />
                  </Suspense>
                )}
                {activeTab === 'danger' && <DangerZone project={selectedProject} />}
              </div>
            </div>
          </>
        ) : projectsLoading ? (
          <div className="flex-1 flex items-center justify-center p-6 bg-bg">
            <div className="text-center">
              <div className="w-8 h-8 border-2 border-accent/30 border-t-accent rounded-full animate-spin mx-auto mb-4" />
              <p className="text-[11px] text-muted font-mono uppercase">Loading workspace...</p>
            </div>
          </div>
        ) : projectsError ? (
          <div className="flex-1 flex items-center justify-center p-6 bg-bg">
            <div className="border border-bad/20 rounded-lg p-8 max-w-sm w-full text-center bg-surface relative">
              <div className="w-12 h-12 rounded bg-bad/10 border border-bad/20 flex items-center justify-center mx-auto mb-5 text-bad">
                <AlertTriangle size={24} />
              </div>
              <h2 className="text-xs font-bold text-ink uppercase tracking-widest mb-1.5">Connection Failed</h2>
              <p className="text-[11px] text-ink-soft mb-5 leading-relaxed font-mono uppercase">
                Could not load project data. Check that the control plane is running.
              </p>
              <button
                onClick={() => window.location.reload()}
                className="px-4 py-2 rounded bg-accent text-accent-on font-bold text-xs transition-colors hover:bg-accent-hover uppercase tracking-wider flex items-center justify-center gap-1.5 mx-auto"
              >
                <RefreshCw size={12} className="stroke-[3]" />
                Retry
              </button>
            </div>
          </div>
        ) : (
          <div className="flex-1 flex items-center justify-center p-6 bg-bg">
            <div className="border border-line rounded-lg p-8 max-w-sm w-full text-center bg-surface relative">
              <div className="w-12 h-12 rounded bg-surface-2 border border-line flex items-center justify-center mx-auto mb-5 text-muted">
                <Box size={24} />
              </div>
              <h2 className="text-xs font-bold text-ink uppercase tracking-widest mb-1.5">No Node Active</h2>
              <p className="text-[11px] text-ink-soft mb-5 leading-relaxed font-mono uppercase">
                {projects.length === 0
                  ? 'Create your first devops target node to begin orchestration.'
                  : 'Select an orchestrated node from the workspace listing.'}
              </p>
              {projects.length === 0 && (
                <button
                  onClick={() => {
                    setEditingProject(null);
                    setShowModal(true);
                  }}
                  className="px-4 py-2 rounded bg-accent text-accent-on font-bold text-xs transition-colors hover:bg-accent-hover uppercase tracking-wider flex items-center justify-center gap-1.5 mx-auto"
                >
                  <Plus size={12} className="stroke-[3]" />
                  Register Node
                </button>
              )}
            </div>
          </div>
        )}
      </main>

      <ProjectModal
        open={showModal}
        onClose={() => {
          setShowModal(false);
          setEditingProject(null);
        }}
        project={editingProject}
      />

      <AestheticsCustomizer
        theme={theme}
        onThemeChange={handleThemeChange}
      />
    </div>
  );
}
