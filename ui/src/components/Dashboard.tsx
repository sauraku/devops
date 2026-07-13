import { useState, useEffect } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import type { Project } from '../types';
import * as api from '../lib/api';
import { useAnimatedNumber } from '../hooks/useAnimatedNumber';
import { useToast } from './Toast';
import { 
  Play, 
  StopCircle, 
  GitBranch, ChevronRight, 
  Server, 
  Shield, 
  AlertTriangle, 
  RefreshCw, 
  ChevronDown, 
  ChevronUp, 
  Cpu, 
  Terminal, 
  History,
  Pause,
  RotateCw,
  Sparkles,
  Square,
  LayoutGrid,
  Table
} from 'lucide-react';

interface DashboardProps {
  project: Project;
}

export function Dashboard({ project }: DashboardProps) {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const [deployBranch, setDeployBranch] = useState(project.branch_name);
  const [containersCollapsed, setContainersCollapsed] = useState(false);
  const [deploysCollapsed, setDeploysCollapsed] = useState(false);
  const [copied, setCopied] = useState<string | null>(null);
  const [dispatchCollapsed, setDispatchCollapsed] = useState(project.auto_apply);
  const [viewMode, setViewMode] = useState<'grid' | 'table'>('grid');

  useEffect(() => {
    setDeployBranch(project.branch_name);
  }, [project.id]);

  const { data: status, error: statusError, isFetching, isLoading } = useQuery({
    queryKey: ['project-status', project.id],
    queryFn: () => api.getProjectStatus(project.id),
    refetchInterval: 10000,
  });

  const { data: envData } = useQuery({
    queryKey: ['env-template', project.id],
    queryFn: () => api.getEnvTemplate(project.id),
    staleTime: 30000,
  });
  const envVars = envData?.variables ?? [];
  const envOverrides = envData?.overrides ?? {};
  const missingEnvVars = envVars.filter((v) => {
    const val = envOverrides[v.key] ?? v.default;
    return !val || val === '' || val === 'change_me';
  });
  const envNotConfigured = envVars.length > 0 && missingEnvVars.length > 0;

  const deployMutation = useMutation({
    mutationFn: () => api.deployProject(project.id, { ref: deployBranch, branch: deployBranch, confirmation: 'deploy' }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['project-status', project.id] });
      toast('Deploy initiated', 'success');
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const abortMutation = useMutation({
    mutationFn: () => api.abortDeploy(project.id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['project-status', project.id] });
      toast('Deploy aborted', 'warn');
    },
    onError: (err: Error) => toast(err.message, 'error'),
  });

  const [actionLoading, setActionLoading] = useState<string | null>(null);

  const containerActionMutation = useMutation({
    mutationFn: ({ service, action }: { service: string; action: string }) => 
      api.containerAction(project.id, service, action),
    onSuccess: (data, variables) => {
      queryClient.invalidateQueries({ queryKey: ['project-status', project.id] });
      toast(data.message || `Container ${variables.service} ${variables.action} successful`, 'success');
    },
    onError: (err: Error) => toast(err.message, 'error'),
    onSettled: () => {
      setActionLoading(null);
    }
  });

  const handleContainerAction = (service: string, action: string) => {
    setActionLoading(`${service}-${action}`);
    containerActionMutation.mutate({ service, action });
  };



  const state = status?.state ?? {};
  const lock = status?.lock;
  const runner = status?.runner ?? { container: 'unknown', state: 'unknown' };
  const isPaused = state.paused === true;

  // Monday Board statistics
  const containerList = status?.containers?.current ? Object.entries(status.containers.current) : [];
  const totalContainers = containerList.length;
  const runningContainers = containerList.filter(([_, s]) => s === 'running').length;
  const runPercent = totalContainers > 0 ? Math.round((runningContainers / totalContainers) * 100) : 0;

  const recentDeploys = status?.recent_deployments ?? [];
  const totalDeploys = recentDeploys.length;
  const successDeploys = recentDeploys.filter(d => d.status === 'success').length;
  const successPercent = totalDeploys > 0 ? Math.round((successDeploys / totalDeploys) * 100) : 0;

  const animatedRunPercent = useAnimatedNumber(runPercent);
  const animatedSuccessPercent = useAnimatedNumber(successPercent);

  return (
    <div className="space-y-6">
      {isLoading ? (
        <div className="flex items-center justify-center py-16">
          <div className="w-8 h-8 border-2 border-accent/30 border-t-accent rounded-full animate-spin" />
        </div>
      ) : (<>
      {statusError && (
        <div className="p-4 rounded border border-bad/20 bg-bad/10 text-bad text-xs font-mono flex items-center justify-between">
          <div className="flex items-center gap-2">
            <AlertTriangle size={14} />
            <span>[ALERT] RE-CONNECT FAILED: {(statusError as Error).message}</span>
          </div>
          <button
            onClick={() => queryClient.invalidateQueries({ queryKey: ['project-status', project.id] })}
            className="text-[10px] underline hover:text-ink font-bold uppercase"
          >
            Retry Connection
          </button>
        </div>
      )}

      {/* Hardware Console Header Strip (Floating Console Cards) */}
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-3 sm:gap-4 select-none">
        {/* Card 1: Runner (System Daemon) */}
        <div className={`border border-line bg-surface rounded-2xl p-3 sm:p-4 flex flex-col justify-between relative overflow-hidden transition-colors duration-300 border-l-4 ${
          lock ? 'border-l-warn/90' :
          runner.state === 'running' || runner.state === 'active' || runner.state === 'online' ? 'border-l-good/90' :
          'border-l-bad/70'
        }`}>
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <span className="relative flex h-2 w-2 shrink-0">
                <span className={`animate-ping absolute inline-flex h-full w-full rounded-full opacity-75 ${
                  lock ? 'bg-warn' :
                  runner.state === 'running' || runner.state === 'active' || runner.state === 'online' ? 'bg-good' :
                  'bg-bad'
                }`}></span>
                <span className={`relative inline-flex rounded-full h-2 w-2 ${
                  lock ? 'bg-warn' :
                  runner.state === 'running' || runner.state === 'active' || runner.state === 'online' ? 'bg-good' :
                  'bg-bad'
                }`}></span>
              </span>
              <span className="text-[9px] font-mono text-muted uppercase tracking-widest truncate">[ SYSTEM DAEMON ]</span>
            </div>
            <div className={`p-1.5 rounded-lg shrink-0 ${
              lock ? 'bg-warn/15 text-warn' :
              runner.state === 'running' || runner.state === 'active' || runner.state === 'online' ? 'bg-good/15 text-good' :
              'bg-bad/15 text-bad'
            }`}>
              <Cpu size={14} className="stroke-[2.5]" />
            </div>
          </div>
          <div className="mt-3">
            <p className={`text-xs sm:text-sm font-bold font-mono tracking-tight uppercase ${
              lock ? 'text-warn' :
              runner.state === 'running' || runner.state === 'active' || runner.state === 'online' ? 'text-good' :
              'text-bad'
            }`}>
              {lock ? 'LOCK_HELD' : runner.state.toUpperCase()}
            </p>
            <p className="text-[9px] sm:text-[10px] text-muted font-mono truncate uppercase mt-1">
              {runner.container || 'offline_state'}
            </p>
          </div>
        </div>

        {/* Card 2: Deploy Gate */}
        <div className={`border border-line bg-surface rounded-2xl p-3 sm:p-4 flex flex-col justify-between relative overflow-hidden transition-colors duration-300 border-l-4 ${
          isPaused ? 'border-l-bad/90' : 'border-l-good/90'
        }`}>
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <span className="relative flex h-2 w-2 shrink-0">
                <span className={`animate-ping absolute inline-flex h-full w-full rounded-full opacity-75 ${
                  isPaused ? 'bg-bad' : 'bg-good'
                }`}></span>
                <span className={`relative inline-flex rounded-full h-2 w-2 ${
                  isPaused ? 'bg-bad' : 'bg-good'
                }`}></span>
              </span>
              <span className="text-[9px] font-mono text-muted uppercase tracking-widest truncate">[ DEPLOY GATE ]</span>
            </div>
            <div className={`p-1.5 rounded-lg shrink-0 ${
              isPaused ? 'bg-bad/15 text-bad' : 'bg-good/15 text-good'
            }`}>
              <Shield size={14} className="stroke-[2.5]" />
            </div>
          </div>
          <div className="mt-3">
            <p className={`text-xs sm:text-sm font-bold font-mono tracking-tight uppercase ${
              isPaused ? 'text-bad' : 'text-good'
            }`}>
              {isPaused ? 'GATE_LOCKED' : 'READY'}
            </p>
            <p className="text-[9px] sm:text-[10px] text-muted font-mono truncate uppercase mt-1">
              {isPaused ? (state.paused_reason as string) : 'accepting webhook trigger'}
            </p>
          </div>
        </div>

        {/* Card 3: Release Version */}
        <div className={`border border-line bg-surface rounded-2xl p-3 sm:p-4 flex flex-col justify-between relative overflow-hidden transition-colors duration-300 border-l-4 ${
          state.last_deployed_commit ? 'border-l-good/90' : 'border-l-warn/90'
        }`}>
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <span className="relative flex h-2 w-2 shrink-0">
                <span className={`animate-ping absolute inline-flex h-full w-full rounded-full opacity-75 ${
                  state.last_deployed_commit ? 'bg-good' : 'bg-warn'
                }`}></span>
                <span className={`relative inline-flex rounded-full h-2 w-2 ${
                  state.last_deployed_commit ? 'bg-good' : 'bg-warn'
                }`}></span>
              </span>
              <span className="text-[9px] font-mono text-muted uppercase tracking-widest truncate">[ RELEASE VERSION ]</span>
            </div>
            <div className={`p-1.5 rounded-lg shrink-0 ${
              state.last_deployed_commit ? 'bg-good/15 text-good' : 'bg-warn/15 text-warn'
            }`}>
              <GitBranch size={14} className="stroke-[2.5]" />
            </div>
          </div>
          <div className="mt-3">
            {state.last_deployed_commit ? (
              <div className="inline-flex items-center gap-1 px-2 py-0.5 rounded bg-good/15 border border-good/25 text-good text-xs font-mono font-bold tracking-wider select-all cursor-pointer hover:bg-accent/20 transition-colors">
                { (state.last_deployed_commit as string).slice(0, 8).toUpperCase() }
              </div>
            ) : state.last_deployed_image_tag ? (
              <div className="inline-flex items-center gap-1 px-2 py-0.5 rounded bg-good/15 border border-good/25 text-good text-xs font-mono font-bold tracking-wider select-all cursor-pointer hover:bg-accent/20 transition-colors">
                { (state.last_deployed_image_tag as string).slice(0, 16) }
              </div>
            ) : (
              <div className="inline-flex items-center gap-1 px-2 py-0.5 rounded bg-warn/15 border border-warn/25 text-warn text-xs font-mono font-bold tracking-wider uppercase select-none">
                NO_RELEASE
              </div>
            )}
            <p className="text-[9px] sm:text-[10px] text-muted font-mono truncate uppercase mt-1">
              {state.last_run_at || 'no execution history'}
            </p>
          </div>
        </div>
      </div>

      {/* Industrial Console Control Panel (Form + Gates) */}
      <div className="border border-line bg-surface rounded-2xl p-4 sm:p-5 space-y-4 sm:space-y-5">
        <div className="flex flex-col xl:flex-row xl:items-center justify-between gap-4">
          <div>
            <div className="flex items-center gap-2">
              <Terminal size={14} className="text-accent" />
              <h3 className="text-xs font-bold uppercase tracking-wider text-ink">Manual Job Dispatcher</h3>
            </div>
            <p className="text-[11px] text-ink-soft font-mono mt-1 leading-relaxed max-w-xl">
              Execute compilation pipelines and orchestrate stacks on the runner node. Specify code reference and confirm execution.
            </p>

            {status?.recent_deployments?.[0] && status.recent_deployments[0].kind === 'deploy' && (
              <div
                onClick={() => { document.getElementById('deploy-history')?.scrollIntoView({ behavior: 'smooth' }); }}
                className={`mt-2 mb-1 text-[10px] font-mono flex items-center gap-2 font-bold cursor-pointer ${
                status.recent_deployments[0].status === 'success' ? 'text-good' :
                status.recent_deployments[0].status === 'failed' ? 'text-bad' : 'text-warn'
              }`}>
                <span>Last deploy:</span>
                <span className="font-bold uppercase">
                  [{status.recent_deployments[0].status === 'success' ? 'OK' : status.recent_deployments[0].status === 'failed' ? 'FAIL' : status.recent_deployments[0].status.toUpperCase()}]
                </span>
                <span className="text-muted">{status.recent_deployments[0].sha?.slice(0, 8) || status.recent_deployments[0].id.slice(0, 8)}</span>
                <span className="text-muted ml-1 underline">view logs</span>
              </div>
            )}
          </div>

          {dispatchCollapsed ? (
            <div onClick={() => setDispatchCollapsed(false)} className="flex items-center gap-2 cursor-pointer text-[10px] font-mono text-muted hover:text-ink-soft transition-colors py-1">
              <ChevronRight size={12} />
              <span>Auto-deploy active on <span className="text-ink-soft font-bold">{project.branch_name}</span> — manual overrides available</span>
            </div>
          ) : (
            <>
              <form
                onSubmit={(e) => {
                  e.preventDefault();
                  const fd = new FormData(e.currentTarget);
                  if (fd.get('confirmation') !== 'deploy') return;
                  deployMutation.mutate();
                }}
                className="flex flex-col sm:flex-row gap-3.5 items-stretch sm:items-end w-full xl:w-auto"
              >
                <div className="sm:w-44">
                  <label className="block text-[9px] font-mono text-muted uppercase tracking-wider mb-1.5">Branch / tag</label>
                  <input
                    name="ref"
                    value={deployBranch}
                    onChange={(e) => setDeployBranch(e.target.value)}
                    className="w-full px-3 py-1.5 rounded bg-bg border border-line text-ink text-xs focus:outline-none focus:border-accent font-mono"
                  />
                </div>
                <div className="sm:w-36">
                  <label className="block text-[9px] font-mono text-muted uppercase tracking-wider mb-1.5">Type 'deploy'</label>
                  <input
                    name="confirmation"
                    required
                    placeholder="deploy"
                    className="w-full px-3 py-1.5 rounded bg-bg border border-line text-ink text-xs focus:outline-none focus:border-accent font-mono"
                  />
                </div>
                <button
                  type="submit"
                  disabled={deployMutation.isPending || isPaused || envNotConfigured}
                  className="px-5 py-2 rounded bg-accent hover:bg-accent-hover text-accent-on font-bold text-xs uppercase tracking-wider transition-colors disabled:opacity-30 disabled:scale-100 flex items-center justify-center gap-1.5 h-[30px] self-stretch sm:self-auto shrink-0 cursor-pointer active:translate-y-[0.5px]"
                  title={envNotConfigured ? 'Configure environment variables before deploying' : ''}
                >
                  <Play size={11} className="fill-current" />
                  {deployMutation.isPending ? 'Executing...' : 'Run Job'}
                </button>
              </form>
              <div onClick={() => setDispatchCollapsed(true)} className="flex items-center gap-2 cursor-pointer text-[10px] font-mono text-muted hover:text-ink-soft transition-colors py-1 mt-2">
                <ChevronRight size={12} className="rotate-90" />
                <span>Collapse manual dispatch</span>
              </div>
            </>
          )}
        </div>

        {/* Buttons Console Strip */}
        <div className="flex flex-wrap gap-2.5 border-t border-line pt-4">
          <button
            onClick={() => {
              if (confirm('Abort the currently running deployment?')) {
                abortMutation.mutate();
              }
            }}
            className="px-3.5 py-1.5 rounded border border-bad text-bad bg-bad/5 hover:bg-bad/15 text-[10px] font-bold uppercase tracking-wider transition-colors active:translate-y-[0.5px]"
          >
            <StopCircle size={11} className="inline mr-1" /> Abort Deployment
          </button>

          <button
            onClick={() => queryClient.invalidateQueries({ queryKey: ['project-status', project.id] })}
            disabled={isFetching}
            className="px-3.5 py-1.5 rounded border border-line text-ink-soft bg-surface-2 hover:bg-surface-3 text-[10px] font-bold uppercase tracking-wider transition-colors ml-auto active:translate-y-[0.5px]"
          >
            <RefreshCw size={11} className={`inline mr-1 ${isFetching ? 'animate-spin' : ''}`} /> Refresh board
          </button>
        </div>
      </div>

      {/* Monday.dev Board Group: Containers */}
      <div className="border border-line bg-surface rounded-2xl overflow-hidden">
        {/* Board Section Header */}
        <div 
          onClick={() => setContainersCollapsed(!containersCollapsed)}
          aria-expanded={!containersCollapsed}
          role="button"
          tabIndex={0}
          className="px-4 py-3 bg-surface-2 border-b border-line flex items-center justify-between cursor-pointer select-none group"
        >
          <div className="flex items-center gap-2">
            <Server size={13} className="text-accent" />
            <h2 className="text-[10px] font-bold tracking-widest uppercase text-ink group-hover:text-accent transition-colors flex items-center gap-1.5">
              CONTAINERS & CONTAINER SERVICES
              <span className="text-[9px] font-mono text-muted normal-case font-bold">({containerList.length} total)</span>
            </h2>
          </div>
          <div className="text-muted group-hover:text-ink-soft transition-colors">
            {containersCollapsed ? <ChevronDown size={14} /> : <ChevronUp size={14} />}
          </div>
        </div>

        {!containersCollapsed && (
          <div>
            {/* View Selector Action Bar */}
            <div className="px-4 py-2.5 bg-surface-2/10 border-b border-line flex items-center justify-between">
              <span className="text-[10px] font-mono text-muted uppercase tracking-wider">
                {viewMode === 'grid' ? 'Bento Grid View' : 'Table List View'}
              </span>
              <div className="flex items-center gap-1 bg-surface-2 p-0.5 rounded-lg border border-line select-none">
                <button
                  onClick={(e) => { e.stopPropagation(); setViewMode('grid'); }}
                  className={`px-2.5 py-1 rounded flex items-center gap-1.5 text-[9px] font-bold uppercase transition-all cursor-pointer ${viewMode === 'grid' ? 'bg-surface text-accent shadow-sm' : 'text-ink-soft hover:text-ink hover:bg-surface-3'}`}
                  title="Bento Grid View"
                >
                  <LayoutGrid size={11} />
                  Grid
                </button>
                <button
                  onClick={(e) => { e.stopPropagation(); setViewMode('table'); }}
                  className={`px-2.5 py-1 rounded flex items-center gap-1.5 text-[9px] font-bold uppercase transition-all cursor-pointer ${viewMode === 'table' ? 'bg-surface text-accent shadow-sm' : 'text-ink-soft hover:text-ink hover:bg-surface-3'}`}
                  title="Table List View"
                >
                  <Table size={11} />
                  Table
                </button>
              </div>
            </div>

            {viewMode === 'grid' ? (
              /* Bento Grid View */
              <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4 p-4">
                {containerList.length > 0 ? (
                  containerList.map(([service, containerState]) => {
                    const health = status?.service_health?.[service];
                    return (
                      <div key={service} className="border border-line bg-surface rounded-xl overflow-hidden hover:border-accent/40 hover:shadow-lg transition-all flex flex-col h-full group">
                        {/* Card Header */}
                        <div className="p-4 bg-surface-2/40 border-b border-line flex items-center justify-between">
                          <div className="flex items-center gap-2">
                            <Server size={14} className="text-muted group-hover:text-accent transition-colors" />
                            <span className="font-mono font-bold text-xs text-ink">{service}</span>
                          </div>
                          {(() => {
                            if (containerState === 'running') {
                              return (
                                <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-[9px] font-bold bg-good/10 text-good border border-good/15 select-none">
                                  <span className="w-1 h-1 rounded-full bg-good animate-pulse"></span>
                                  RUNNING
                                </span>
                              );
                            }
                            if (containerState === 'paused') {
                              return (
                                <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-[9px] font-bold bg-warn/10 text-warn border border-warn/15 select-none">
                                  <span className="w-1 h-1 rounded-full bg-warn animate-pulse"></span>
                                  PAUSED
                                </span>
                              );
                            }
                            return (
                              <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-[9px] font-bold bg-surface-2 text-ink-soft border border-line select-none">
                                <span className="w-1 h-1 rounded-full bg-muted"></span>
                                {containerState.toUpperCase()}
                              </span>
                            );
                          })()}
                        </div>

                        {/* Card Body */}
                        <div className="p-4 flex-grow flex flex-col gap-3 font-mono text-[11px]">
                          {/* Container Ref Row */}
                          <div className="flex items-center justify-between border-b border-line/40 pb-2">
                            <span className="text-muted text-[10px]">CONTAINER REF</span>
                            {(() => {
                              const containerRef = health?.detail?.includes('container') 
                                ? health.detail.split(' ')[0] 
                                : `${project.id}_${service}`;
                              return (
                                <span 
                                  onClick={() => { navigator.clipboard.writeText(containerRef).then(() => { setCopied(containerRef); setTimeout(() => setCopied(null), 800); }); }}
                                  className="text-ink-soft cursor-pointer hover:text-accent font-bold truncate max-w-[150px]"
                                  title="Click to copy reference"
                                >
                                  {copied === containerRef ? 'COPIED' : containerRef}
                                </span>
                              );
                            })()}
                          </div>

                          {/* Health state Row */}
                          <div className="flex items-center justify-between border-b border-line/40 pb-2">
                            <span className="text-muted text-[10px]">HEALTH STATE</span>
                            {(() => {
                              const statusStr = health?.status ?? 'unknown';
                              if (statusStr === 'healthy') {
                                  return (
                                    <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-[9px] font-bold bg-good/10 text-good border border-good/15 select-none">
                                      HEALTHY
                                    </span>
                                  );
                              }
                              if (statusStr === 'unhealthy') {
                                  return (
                                    <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-[9px] font-bold bg-bad/10 text-bad border border-bad/15 select-none">
                                      UNHEALTHY
                                    </span>
                                  );
                              }
                              return (
                                <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-[9px] font-bold bg-surface-2 text-ink-soft border border-line select-none">
                                  {statusStr.toUpperCase()}
                                </span>
                              );
                            })()}
                          </div>

                          {/* Details Row */}
                          <div className="flex flex-col gap-1">
                            <span className="text-muted text-[10px]">DIAGNOSTIC LOG</span>
                            <div className="bg-surface-2/60 p-2 rounded border border-line text-[10px] text-ink-soft break-all font-mono leading-relaxed max-h-16 overflow-y-auto">
                              {health?.detail ?? 'No diagnostic detail logs collected.'}
                            </div>
                          </div>
                        </div>

                        {/* Card Actions Footer */}
                        <div className="p-4 bg-surface-2/20 border-t border-line flex flex-col gap-2">
                          {/* Row 1: Media Control buttons */}
                          <div className="flex items-center gap-2 select-none">
                            {containerState === 'running' || containerState === 'paused' ? (
                              <button
                                disabled={actionLoading !== null}
                                onClick={() => handleContainerAction(service, 'stop')}
                                className="flex-1 bg-bad/10 text-bad border border-bad/20 hover:bg-bad/20 disabled:opacity-40 py-2 px-3 rounded-lg flex items-center justify-center gap-1.5 text-xs font-mono font-bold transition-all cursor-pointer active:scale-[0.98]"
                              >
                                {actionLoading === `${service}-stop` ? (
                                  <span className="w-3.5 h-3.5 border-2 border-bad border-t-transparent rounded-full animate-spin inline-block"></span>
                                ) : (
                                  <Square size={13} className="fill-bad/20" />
                                )}
                                STOP
                              </button>
                            ) : (
                              <button
                                disabled={actionLoading !== null}
                                onClick={() => handleContainerAction(service, 'start')}
                                className="w-full bg-good/10 text-good border border-good/20 hover:bg-good/20 disabled:opacity-40 py-2 px-3 rounded-lg flex items-center justify-center gap-1.5 text-xs font-mono font-bold transition-all cursor-pointer active:scale-[0.98]"
                              >
                                {actionLoading === `${service}-start` ? (
                                  <span className="w-3.5 h-3.5 border-2 border-good border-t-transparent rounded-full animate-spin inline-block"></span>
                                ) : (
                                  <Play size={13} className="fill-good/20" />
                                )}
                                START
                              </button>
                            )}

                            {containerState === 'paused' ? (
                              <button
                                disabled={actionLoading !== null}
                                onClick={() => handleContainerAction(service, 'resume')}
                                className="flex-1 bg-good/10 text-good border border-good/20 hover:bg-good/20 disabled:opacity-40 py-2 px-3 rounded-lg flex items-center justify-center gap-1.5 text-xs font-mono font-bold transition-all cursor-pointer active:scale-[0.98]"
                              >
                                {actionLoading === `${service}-resume` ? (
                                  <span className="w-3.5 h-3.5 border-2 border-good border-t-transparent rounded-full animate-spin inline-block"></span>
                                ) : (
                                  <Play size={13} className="fill-good/20" />
                                )}
                                RESUME
                              </button>
                            ) : containerState === 'running' ? (
                              <button
                                disabled={actionLoading !== null}
                                onClick={() => handleContainerAction(service, 'pause')}
                                className="flex-1 bg-warn/10 text-warn border border-warn/20 hover:bg-warn/20 disabled:opacity-40 py-2 px-3 rounded-lg flex items-center justify-center gap-1.5 text-xs font-mono font-bold transition-all cursor-pointer active:scale-[0.98]"
                              >
                                {actionLoading === `${service}-pause` ? (
                                  <span className="w-3.5 h-3.5 border-2 border-warn border-t-transparent rounded-full animate-spin inline-block"></span>
                                ) : (
                                  <Pause size={13} className="fill-warn/20" />
                                )}
                                PAUSE
                              </button>
                            ) : null}
                          </div>

                          {/* Row 2: Maintenance buttons (Restart, Recreate) */}
                          <div className="flex items-center gap-2 select-none">
                            {(containerState === 'running' || containerState === 'paused') && (
                              <button
                                disabled={actionLoading !== null}
                                onClick={() => handleContainerAction(service, 'restart')}
                                className="flex-1 bg-surface-2 text-ink-soft hover:bg-surface-3 hover:text-ink border border-line disabled:opacity-40 py-2 px-3 rounded-lg flex items-center justify-center gap-1.5 text-xs font-mono font-bold transition-all cursor-pointer active:scale-[0.98]"
                              >
                                {actionLoading === `${service}-restart` ? (
                                  <span className="w-3.5 h-3.5 border-2 border-ink-soft border-t-transparent rounded-full animate-spin inline-block"></span>
                                ) : (
                                  <RotateCw size={13} />
                                )}
                                RESTART
                              </button>
                            )}

                            <button
                              disabled={actionLoading !== null}
                              onClick={() => handleContainerAction(service, 'recreate')}
                              className={`${(containerState === 'running' || containerState === 'paused') ? 'flex-1' : 'w-full'} bg-accent/10 text-accent border border-accent/20 hover:bg-accent/20 disabled:opacity-40 py-2 px-3 rounded-lg flex items-center justify-center gap-1.5 text-xs font-mono font-bold transition-all cursor-pointer active:scale-[0.98]`}
                            >
                              {actionLoading === `${service}-recreate` ? (
                                <span className="w-3.5 h-3.5 border-2 border-accent border-t-transparent rounded-full animate-spin inline-block"></span>
                              ) : (
                                <Sparkles size={13} className="fill-accent/20" />
                              )}
                              RECREATE
                            </button>
                          </div>
                        </div>
                      </div>
                    );
                  })
                ) : (
                  <div className="col-span-full py-8 text-center text-xs text-muted font-mono uppercase">
                    NO DOCKER CONTAINERS FOUND IN PROJECT SCOPE.
                  </div>
                )}
              </div>
            ) : (
              /* Table List View */
              <div className="overflow-x-auto">
                <table className="w-full text-left border-collapse min-w-[700px] border-b border-line">
                  <thead>
                    <tr className="border-b border-line text-[9px] text-muted uppercase tracking-widest font-bold bg-surface-3/30">
                      <th className="py-2.5 px-4 w-44 border-r border-line">Service Name</th>
                      <th className="py-2.5 px-4 border-r border-line">Container Ref</th>
                      <th className="py-2.5 px-4 w-36 text-center border-r border-line">Status</th>
                      <th className="py-2.5 px-4 w-36 text-center border-r border-line">Health check</th>
                      <th className="py-2.5 px-4 w-60 text-center">Actions</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-line font-mono text-xs">
                    {containerList.length > 0 ? (
                      containerList.map(([service, containerState]) => {
                        const health = status?.service_health?.[service];
                        
                        return (
                          <tr key={service} className="hover:bg-surface-2/20 transition-colors group">
                            <td className="py-3 px-4 font-bold text-ink border-r border-line">
                              {service}
                            </td>
                            <td className="py-3 px-4 text-ink-soft border-r border-line text-[11px] cursor-pointer select-all">
                              {(() => {
                                const containerRef = health?.detail?.includes('container') 
                                  ? health.detail.split(' ')[0] 
                                  : `${project.id}_${service}`;
                                return (
                                  <span onClick={() => { navigator.clipboard.writeText(containerRef).then(() => { setCopied(containerRef); setTimeout(() => setCopied(null), 800); }); }}>
                                    {copied === containerRef ? 'COPIED' : containerRef}
                                  </span>
                                );
                              })()}
                            </td>
                            <td className="py-3 px-4 border-r border-line w-36">
                              <div className="flex items-center justify-center">
                                {(() => {
                                  if (containerState === 'running') {
                                    return (
                                      <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-[10px] font-bold bg-good/10 text-good border border-good/20 select-none">
                                        <span className="w-1.5 h-1.5 rounded-full bg-good animate-pulse"></span>
                                        RUNNING
                                      </span>
                                    );
                                  }
                                  if (containerState === 'paused') {
                                    return (
                                      <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-[10px] font-bold bg-warn/10 text-warn border border-warn/20 select-none">
                                        <span className="w-1.5 h-1.5 rounded-full bg-warn animate-pulse"></span>
                                        PAUSED
                                      </span>
                                    );
                                  }
                                  return (
                                    <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-[10px] font-bold bg-surface-2 text-ink-soft border border-line select-none">
                                      <span className="w-1.5 h-1.5 rounded-full bg-muted"></span>
                                      {containerState.toUpperCase()}
                                    </span>
                                  );
                                })()}
                              </div>
                            </td>
                            <td className="py-3 px-4 border-r border-line w-36">
                              <div className="flex items-center justify-center">
                                {(() => {
                                  const statusStr = health?.status ?? 'unknown';
                                  if (statusStr === 'healthy') {
                                    return (
                                      <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-[10px] font-bold bg-good/10 text-good border border-good/20 select-none">
                                        <span className="w-1.5 h-1.5 rounded-full bg-good"></span>
                                        HEALTHY
                                      </span>
                                    );
                                  }
                                  if (statusStr === 'unhealthy') {
                                    return (
                                      <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-[10px] font-bold bg-bad/10 text-bad border border-bad/20 select-none">
                                        <span className="w-1.5 h-1.5 rounded-full bg-bad"></span>
                                        UNHEALTHY
                                      </span>
                                    );
                                  }
                                  return (
                                    <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-[10px] font-bold bg-surface-2 text-ink-soft border border-line select-none">
                                      <span className="w-1.5 h-1.5 rounded-full bg-muted"></span>
                                      {statusStr.toUpperCase()}
                                    </span>
                                  );
                                })()}
                              </div>
                            </td>
                            <td className="py-2 px-3">
                              <div className="flex items-center justify-center gap-2">
                                {/* Media Controls */}
                                <div className="flex items-center gap-1 select-none">
                                  {containerState === 'running' || containerState === 'paused' ? (
                                    <button
                                      disabled={actionLoading !== null}
                                      onClick={() => handleContainerAction(service, 'stop')}
                                      className="bg-bad/10 text-bad border border-bad/20 hover:bg-bad/20 disabled:opacity-40 px-2 py-1.5 rounded-lg flex items-center justify-center gap-1 text-[10px] font-mono font-bold transition-all cursor-pointer active:scale-95"
                                    >
                                      {actionLoading === `${service}-stop` ? (
                                        <span className="w-3 h-3 border-2 border-bad border-t-transparent rounded-full animate-spin inline-block"></span>
                                      ) : (
                                        <Square size={11} className="fill-bad/20" />
                                      )}
                                      STOP
                                    </button>
                                  ) : (
                                    <button
                                      disabled={actionLoading !== null}
                                      onClick={() => handleContainerAction(service, 'start')}
                                      className="bg-good/10 text-good border border-good/20 hover:bg-good/20 disabled:opacity-40 px-2 py-1.5 rounded-lg flex items-center justify-center gap-1 text-[10px] font-mono font-bold transition-all cursor-pointer active:scale-95"
                                    >
                                      {actionLoading === `${service}-start` ? (
                                        <span className="w-3 h-3 border-2 border-good border-t-transparent rounded-full animate-spin inline-block"></span>
                                      ) : (
                                        <Play size={11} className="fill-good/20" />
                                      )}
                                      START
                                    </button>
                                  )}

                                  {containerState === 'paused' ? (
                                    <button
                                      disabled={actionLoading !== null}
                                      onClick={() => handleContainerAction(service, 'resume')}
                                      className="bg-good/10 text-good border border-good/20 hover:bg-good/20 disabled:opacity-40 px-2 py-1.5 rounded-lg flex items-center justify-center gap-1 text-[10px] font-mono font-bold transition-all cursor-pointer active:scale-95"
                                    >
                                      {actionLoading === `${service}-resume` ? (
                                        <span className="w-3.5 h-3.5 border-2 border-good border-t-transparent rounded-full animate-spin inline-block"></span>
                                      ) : (
                                        <Play size={11} className="fill-good/20" />
                                      )}
                                      RESUME
                                    </button>
                                  ) : containerState === 'running' ? (
                                    <button
                                      disabled={actionLoading !== null}
                                      onClick={() => handleContainerAction(service, 'pause')}
                                      className="bg-warn/10 text-warn border border-warn/20 hover:bg-warn/20 disabled:opacity-40 px-2 py-1.5 rounded-lg flex items-center justify-center gap-1 text-[10px] font-mono font-bold transition-all cursor-pointer active:scale-95"
                                    >
                                      {actionLoading === `${service}-pause` ? (
                                        <span className="w-3 h-3 border-2 border-warn border-t-transparent rounded-full animate-spin inline-block"></span>
                                      ) : (
                                        <Pause size={11} className="fill-warn/20" />
                                      )}
                                      PAUSE
                                    </button>
                                  ) : null}
                                </div>

                                {/* Divider line */}
                                {(containerState === 'running' || containerState === 'paused') && (
                                  <span className="w-[1px] h-5 bg-line"></span>
                                )}

                                {/* Maintenance Controls */}
                                <div className="flex items-center gap-1 select-none">
                                  {(containerState === 'running' || containerState === 'paused') && (
                                    <button
                                      disabled={actionLoading !== null}
                                      onClick={() => handleContainerAction(service, 'restart')}
                                      className="bg-surface-2 text-ink-soft hover:bg-surface-3 hover:text-ink border border-line disabled:opacity-40 px-2 py-1.5 rounded-lg flex items-center justify-center gap-1 text-[10px] font-mono font-bold transition-all cursor-pointer active:scale-95"
                                    >
                                      {actionLoading === `${service}-restart` ? (
                                        <span className="w-3 h-3 border-2 border-ink-soft border-t-transparent rounded-full animate-spin inline-block"></span>
                                      ) : (
                                        <RotateCw size={11} />
                                      )}
                                      RESTART
                                    </button>
                                  )}

                                  <button
                                    disabled={actionLoading !== null}
                                    onClick={() => handleContainerAction(service, 'recreate')}
                                    className="bg-accent/10 text-accent border border-accent/20 hover:bg-accent/20 disabled:opacity-40 px-2 py-1.5 rounded-lg flex items-center justify-center gap-1 text-[10px] font-mono font-bold transition-all cursor-pointer active:scale-95"
                                  >
                                    {actionLoading === `${service}-recreate` ? (
                                      <span className="w-3 h-3 border-2 border-accent border-t-transparent rounded-full animate-spin inline-block"></span>
                                    ) : (
                                      <Sparkles size={11} className="fill-accent/20" />
                                    )}
                                    RECREATE
                                  </button>
                                </div>
                              </div>
                            </td>
                          </tr>
                        );
                      })
                    ) : (
                      <tr>
                        <td colSpan={5} className="py-8 px-4 text-xs text-center text-muted font-mono uppercase">NO DOCKER CONTAINERS FOUND IN PROJECT SCOPE.</td>
                      </tr>
                    )}
                  </tbody>
                  {/* Monday.dev Column Proportions/Percentages Summary Footer */}
                  {containerList.length > 0 && (
                    <tfoot>
                      <tr className="bg-surface-3/30">
                        <td className="py-3 px-4 text-[9px] text-muted uppercase font-bold border-r border-line">Summary / Stats</td>
                        <td className="py-3 px-4 text-[10px] text-ink-soft font-bold border-r border-line uppercase">Running: {animatedRunPercent}%</td>
                        <td className="p-0 border-r border-line h-12 w-36">
                          <div className="w-full h-full flex flex-col justify-center items-center px-4 bg-surface-2/20">
                            <div className="w-full h-2 bg-surface-3 rounded-none overflow-hidden flex">
                              <div style={{ width: `${animatedRunPercent}%` }} className="bg-good"></div>
                              <div style={{ width: `${100 - animatedRunPercent}%` }} className="bg-bad"></div>
                            </div>
                            <span className="text-[8px] text-muted font-bold mt-1 uppercase tracking-wider">{animatedRunPercent}% run</span>
                          </div>
                        </td>
                        <td className="p-0 border-r border-line h-12 w-36">
                          <div className="w-full h-full flex flex-col justify-center items-center px-4 bg-surface-2/20">
                            {(() => {
                              const healthCount = containerList.filter(([s]) => status?.service_health?.[s]?.status === 'healthy').length;
                              const healthPercent = Math.round((healthCount / totalContainers) * 100);
                              return (
                                <>
                                  <div className="w-full h-2 bg-surface-3 rounded-none overflow-hidden flex">
                                    <div style={{ width: `${healthPercent}%` }} className="bg-good"></div>
                                    <div style={{ width: `${100 - healthPercent}%` }} className="bg-bad"></div>
                                  </div>
                                  <span className="text-[8px] text-muted font-bold mt-1 uppercase tracking-wider">{healthPercent}% healthy</span>
                                </>
                              );
                            })()}
                          </div>
                        </td>
                        <td className="py-3 px-4"></td>
                      </tr>
                    </tfoot>
                  )}
                </table>
              </div>
            )}
          </div>
        )}
      </div>

      {/* Monday.dev Board Group: Deployments History */}
      <div id="deploy-history" className="border border-line bg-surface rounded-2xl overflow-hidden">
        {/* Board Section Header */}
        <div 
          onClick={() => setDeploysCollapsed(!deploysCollapsed)}
          aria-expanded={!deploysCollapsed}
          role="button"
          tabIndex={0}
          className="px-4 py-3 bg-surface-2 border-b border-line flex items-center justify-between cursor-pointer select-none group"
        >
          <div className="flex items-center gap-2">
            <History size={13} className="text-accent" />
            <h2 className="text-[10px] font-bold tracking-widest uppercase text-ink group-hover:text-accent transition-colors flex items-center gap-1.5">
              DEPLOYMENT RUN LOGS & WORKFLOWS
              <span className="text-[9px] font-mono text-muted normal-case font-bold">({recentDeploys.length} runs)</span>
            </h2>
          </div>
          <div className="text-muted group-hover:text-ink-soft transition-colors">
            {deploysCollapsed ? <ChevronDown size={14} /> : <ChevronUp size={14} />}
          </div>
        </div>

        {!deploysCollapsed && (
          <div className="overflow-x-auto">
            <table className="w-full text-left border-collapse min-w-[650px] border-b border-line">
              <thead>
                <tr className="border-b border-line text-[9px] text-muted uppercase tracking-widest font-bold bg-surface-3/30">
                  <th className="py-2.5 px-4 w-44 border-r border-line">Deploy ID</th>
                  <th className="py-2.5 px-4 border-r border-line">Commit / Reference</th>
                  <th className="py-2.5 px-4 w-28 border-r border-line">Branch</th>
                  <th className="py-2.5 px-4 w-36 text-center border-r border-line">Status</th>
                  <th className="py-2.5 px-4 w-32 font-mono">Timestamp</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line font-mono text-xs">
                {recentDeploys.length > 0 ? (
                  recentDeploys.map((d) => {
                    
                    // Monday cells solid status mapping (no border spacing, full layout fill)
                    const getStatusClass = (statusStr: string) => {
                      if (statusStr === 'success') return 'bg-good text-accent-on font-bold';
                      if (statusStr === 'failed') return 'bg-bad text-accent-on font-bold';
                      if (statusStr === 'running') return 'bg-warn text-accent-on font-bold';
                      return 'bg-muted text-accent-on font-bold';
                    };

                    return (
                      <tr key={d.id} className="hover:bg-surface-2/20 transition-colors group">
                        <td className="py-3 px-4 font-bold text-accent border-r border-line cursor-pointer select-all" onClick={() => { navigator.clipboard.writeText(d.id).then(() => { setCopied(d.id); setTimeout(() => setCopied(null), 800); }); }}>
                          {copied === d.id ? 'COPIED' : d.id.slice(0, 8).toUpperCase()}
                        </td>
                        <td className="py-3 px-4 text-ink border-r border-line">
                          <div className="truncate max-w-sm font-sans font-semibold text-[11px]" title={d.commit_message}>
                            {d.commit_message || 'Manual Console Execute'}
                          </div>
                          {d.sha && (
                            <div className="text-[9px] text-muted font-mono mt-0.5 select-all uppercase cursor-pointer" onClick={() => { navigator.clipboard.writeText(d.sha).then(() => { setCopied(d.sha); setTimeout(() => setCopied(null), 800); }); }}>
                              {copied === d.sha ? 'COPIED' : `COMMIT_SHA: ${d.sha.slice(0, 12)}`}
                            </div>
                          )}
                        </td>
                        <td className="py-3 px-4 text-ink-soft select-all border-r border-line text-[11px]">
                          {d.branch}
                        </td>
                        <td className="p-0 border-r border-line h-11 w-36">
                          <div className={`w-full h-full flex items-center justify-center text-[10px] uppercase tracking-wider select-none ${getStatusClass(d.status)}`}>
                            {d.status === 'success' ? 'DONE' : d.status === 'failed' ? 'STUCK' : d.status === 'running' ? 'WORKING' : d.status.toUpperCase()}
                          </div>
                        </td>
                        <td className="py-3 px-4 text-muted text-[10px]">
                          {d.finished_at ? new Date(d.finished_at).toLocaleTimeString() : 'RUNNING...'}
                        </td>
                      </tr>
                    );
                  })
                ) : (
                  <tr>
                    <td colSpan={5} className="py-8 px-4 text-xs text-center text-muted font-mono uppercase">NODE HISTORY EMPTY.</td>
                  </tr>
                )}
              </tbody>
              {/* Monday.dev Column Proportions/Percentages Summary Footer */}
              {recentDeploys.length > 0 && (
                <tfoot>
                  <tr className="bg-surface-3/30">
                    <td className="py-3 px-4 text-[9px] text-muted uppercase font-bold border-r border-line">Summary / Stats</td>
                    <td className="py-3 px-4 text-[10px] text-ink-soft font-bold border-r border-line uppercase">SUCCESS_RATE: {animatedSuccessPercent}%</td>
                    <td className="py-3 px-4 border-r border-line"></td>
                    <td className="p-0 border-r border-line h-12 w-36">
                      <div className="w-full h-full flex flex-col justify-center items-center px-4 bg-surface-2/20">
                        <div className="w-full h-2 bg-surface-3 rounded-none overflow-hidden flex">
                          <div style={{ width: `${animatedSuccessPercent}%` }} className="bg-good"></div>
                          {recentDeploys.filter(d => d.status === 'failed').length > 0 && (
                            <div 
                              style={{ width: `${Math.round((recentDeploys.filter(d => d.status === 'failed').length / totalDeploys) * 100)}%` }} 
                              className="bg-bad"
                            ></div>
                          )}
                          {recentDeploys.filter(d => d.status === 'running').length > 0 && (
                            <div 
                              style={{ width: `${Math.round((recentDeploys.filter(d => d.status === 'running').length / totalDeploys) * 100)}%` }} 
                              className="bg-warn"
                            ></div>
                          )}
                        </div>
                        <span className="text-[8px] text-muted font-bold mt-1 uppercase tracking-wider">{animatedSuccessPercent}% success</span>
                      </div>
                    </td>
                    <td className="py-3 px-4"></td>
                  </tr>
                </tfoot>
              )}
            </table>
          </div>
        )}
      </div>
      </> )}
    </div>
  );
}
