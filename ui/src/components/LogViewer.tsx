import { useState, useEffect, useRef, useCallback, useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import * as api from '../lib/api';
import { useWebSocket } from '../hooks/useWebSocket';
import type { Project } from '../types';
import { Download, Wifi, WifiOff, Loader2, PlayCircle, Copy, Check, FileText, Container } from 'lucide-react';

type LogSource = 'deployment' | 'container' | 'file' | 'container-file';

interface LogViewerProps {
  project: Project;
  deployId: string;
  onDeploySelect: (id: string) => void;
}

export function LogViewer({ project, deployId, onDeploySelect }: LogViewerProps) {
  const [logSource, setLogSource] = useState<LogSource>('deployment');
  const [activeDeployId, setActiveDeployId] = useState(deployId);
  const [activeService, setActiveService] = useState('');
  const [activeLogFile, setActiveLogFile] = useState('');
  const [activeContainerFileService, setActiveContainerFileService] = useState('');
  const [logs, setLogs] = useState<string[]>([]);
  const [isStreaming, setIsStreaming] = useState(false);
  const [copied, setCopied] = useState(false);
  const [logFilter, setLogFilter] = useState('');
  const [logLevel, setLogLevel] = useState<'all' | 'error' | 'warn' | 'info'>('all');
  const logEndRef = useRef<HTMLDivElement>(null);

  const { data: projectStatus } = useQuery({
    queryKey: ['project-status', project.id],
    queryFn: () => api.getProjectStatus(project.id),
    refetchInterval: 10000,
  });

  const deployments = projectStatus?.recent_deployments ?? [];
  const containerStates = projectStatus?.containers?.current ?? {};
  const services = Object.keys(containerStates);
  const logDir = projectStatus?.log_dir ?? '';

  const effectiveDeployId = activeDeployId || deployments[0]?.id || '';

  useEffect(() => {
    if (!activeDeployId) {
      const running = deployments.find(d => d.status === 'running' || d.status === 'pending');
      if (running) {
        setActiveDeployId(running.id);
        onDeploySelect(running.id);
        setIsStreaming(false);
      }
    }
  }, [deployments, activeDeployId, onDeploySelect]);

  // Fetch deployment logs
  const { data: historicalLog, isFetching: logLoading } = useQuery({
    queryKey: ['deployment-log', project.id, effectiveDeployId],
    queryFn: () => api.getDeploymentLog(project.id, effectiveDeployId),
    enabled: logSource === 'deployment' && !!effectiveDeployId && !isStreaming,
  });

  // Fetch container logs
  const { data: containerLogData, isFetching: containerLogLoading } = useQuery({
    queryKey: ['container-logs', project.id, activeService],
    queryFn: () => api.getContainerLogs(project.id, activeService, 500),
    enabled: logSource === 'container' && !!activeService,
    refetchInterval: 5000,
  });

  // Fetch log files list
  const { data: logFilesData } = useQuery({
    queryKey: ['project-logs', project.id],
    queryFn: () => api.getProjectLogs(project.id),
    enabled: logSource === 'file',
    refetchInterval: 10000,
  });

  const logFiles = logFilesData?.logs ?? [];

  // Fetch container-internal log files list
  const { data: containerLogFilesData } = useQuery({
    queryKey: ['container-log-files', project.id],
    queryFn: () => api.getContainerLogFiles(project.id),
    enabled: logSource === 'container-file',
    refetchInterval: 10000,
  });

  const containerLogFiles = containerLogFilesData?.logs ?? [];

  // Fetch specific container-internal log file content
  const { data: containerFileContent, isFetching: containerFileLoading } = useQuery({
    queryKey: ['container-log-file-content', project.id, activeContainerFileService],
    queryFn: () => api.getContainerLogFileContent(project.id, activeContainerFileService),
    enabled: logSource === 'container-file' && !!activeContainerFileService,
    refetchInterval: 5000,
  });
  const { data: logFileContent, isFetching: logFileLoading } = useQuery({
    queryKey: ['log-file-content', project.id, activeLogFile],
    queryFn: () => api.getProjectLogContent(project.id, activeLogFile),
    enabled: logSource === 'file' && !!activeLogFile,
    refetchInterval: 5000,
  });

  // Set logs based on source
  useEffect(() => {
    if (logSource === 'deployment' && historicalLog && !isStreaming) {
      setLogs(historicalLog.split('\n').filter(Boolean));
    }
  }, [historicalLog, isStreaming, logSource]);

  useEffect(() => {
    if (logSource === 'container' && containerLogData) {
      setLogs(containerLogData.split('\n').filter(Boolean));
    }
  }, [containerLogData, logSource]);

  useEffect(() => {
    if (logSource === 'file' && logFileContent) {
      setLogs(logFileContent.split('\n').filter(Boolean));
    }
  }, [logFileContent, logSource]);

  useEffect(() => {
    if (logSource === 'container-file' && containerFileContent) {
      setLogs(containerFileContent.split('\n').filter(Boolean));
    }
  }, [containerFileContent, logSource]);

  // Auto-select first service if none selected
  useEffect(() => {
    if (logSource === 'container' && !activeService && services.length > 0) {
      setActiveService(services[0]);
    }
  }, [logSource, services, activeService]);

  // Auto-select first log file if none selected
  useEffect(() => {
    if (logSource === 'file' && !activeLogFile && logFiles.length > 0) {
      setActiveLogFile(logFiles[0].name);
    }
  }, [logSource, logFiles, activeLogFile]);

  // Auto-select first container-internal log file if none selected
  useEffect(() => {
    if (logSource === 'container-file' && !activeContainerFileService && containerLogFiles.length > 0) {
      setActiveContainerFileService(containerLogFiles[0].service);
    }
  }, [logSource, containerLogFiles, activeContainerFileService]);

  const wsUrl = logSource === 'deployment' && activeDeployId
    ? `/api/projects/${encodeURIComponent(project.id)}/deployments/${encodeURIComponent(activeDeployId)}/stream?name=${encodeURIComponent(activeDeployId)}.log`
    : null;

  const handleMessage = useCallback((data: string) => {
    const lines = data.split('\n').filter(Boolean);
    setLogs((prev) => {
      const next = [...prev, ...lines];
      return next.length > 10000 ? next.slice(-10000) : next;
    });
  }, []);

  const handleStatusChange = useCallback((connected: boolean) => {
    setIsStreaming(connected);
  }, []);

  const { reconnect } = useWebSocket(wsUrl, handleMessage, handleStatusChange);

  useEffect(() => {
    logEndRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [logs]);

  const handleCopy = () => {
    const text = logs.join('\n');
    if (navigator.clipboard) {
      navigator.clipboard.writeText(text).then(() => {
        setCopied(true);
        setTimeout(() => setCopied(false), 2000);
      }).catch(() => fallbackCopy(text));
    } else {
      fallbackCopy(text);
    }
  };

  const fallbackCopy = (text: string) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    document.execCommand('copy');
    document.body.removeChild(ta);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  const handleDownload = async () => {
    let text = logs.join('\n');
    if (!text && effectiveDeployId && logSource === 'deployment') {
      try {
        text = await api.getDeploymentLog(project.id, effectiveDeployId);
      } catch { /* ignore */ }
    }
    if (!text) return;
    const blob = new Blob([text], { type: 'text/plain' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `${logSource}-${Date.now()}.log`;
    a.click();
    URL.revokeObjectURL(url);
  };

  const filteredLogs = useMemo(() => {
    let result = logs;
    if (logLevel === 'error') result = result.filter(l => l.toLowerCase().includes('error') || l.toLowerCase().includes('failed') || l.startsWith('[ERROR]'));
    else if (logLevel === 'warn') result = result.filter(l => l.toLowerCase().includes('warning') || l.startsWith('[WARN]'));
    else if (logLevel === 'info') result = result.filter(l => {
      const isErr = l.toLowerCase().includes('error') || l.toLowerCase().includes('failed') || l.startsWith('[ERROR]');
      const isWrn = l.toLowerCase().includes('warning') || l.startsWith('[WARN]');
      return !isErr && !isWrn;
    });
    if (logFilter) {
      const lower = logFilter.toLowerCase();
      result = result.filter(l => l.toLowerCase().includes(lower));
    }
    return result;
  }, [logs, logFilter, logLevel]);

  const isLoading = (logSource === 'deployment' && logLoading) ||
    (logSource === 'container' && containerLogLoading) ||
    (logSource === 'file' && logFileLoading) ||
    (logSource === 'container-file' && containerFileLoading);

  return (
    <div className="flex flex-col gap-4 min-h-0 flex-1">
      {/* Log Directory Info */}
      {logDir && (
        <div className="glass-panel border border-line rounded-2xl p-3 shadow-md">
          <div className="flex items-center gap-2 text-[10px] font-mono text-muted">
            <FileText size={12} className="text-accent shrink-0" />
            <span className="uppercase tracking-wider font-bold">Log Directory:</span>
            <code className="text-ink-soft select-all">{logDir}</code>
          </div>
        </div>
      )}

      {/* Controls Strip */}
      <div className="glass-panel border border-line rounded-2xl p-5 shadow-md">
        <div className="flex flex-col md:flex-row md:items-end justify-between gap-4">
          <div className="flex-1 space-y-3">
            {/* Log Source Selector */}
            <div className="flex items-center gap-2">
              {([
                { id: 'deployment' as LogSource, label: 'Deployment Logs', icon: PlayCircle },
                { id: 'container' as LogSource, label: 'Container Logs', icon: Container },
                { id: 'file' as LogSource, label: 'Log Files', icon: FileText },
                { id: 'container-file' as LogSource, label: 'Container Log Files', icon: FileText },
              ]).map(src => {
                const Icon = src.icon;
                const active = logSource === src.id;
                return (
                  <button
                    key={src.id}
                    onClick={() => { setLogSource(src.id); setLogs([]); setIsStreaming(false); }}
                    className={`px-3 py-1.5 rounded-xl text-[10px] font-bold uppercase tracking-wider transition-all flex items-center gap-1.5 border ${
                      active
                        ? 'bg-accent/15 text-accent border-accent/25'
                        : 'bg-surface-2/65 text-ink-soft border-line hover:bg-surface-2'
                    }`}
                  >
                    <Icon size={12} />
                    {src.label}
                  </button>
                );
              })}
            </div>

            {/* Source-specific selector */}
            {logSource === 'deployment' && (
              <div>
                <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5 flex items-center gap-1">
                  <PlayCircle size={12} className="text-accent" /> Deployment
                </label>
                <select
                  value={effectiveDeployId}
                  onChange={(e) => { 
                    setActiveDeployId(e.target.value); 
                    onDeploySelect(e.target.value); 
                    setLogs([]); 
                    setIsStreaming(false); 
                  }}
                  className="w-full px-3.5 py-2 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all font-mono"
                >
                  {deployments.length === 0 ? (
                    <option value="">No deployments detected</option>
                  ) : (
                    deployments.map((d) => (
                      <option key={d.id} value={d.id}>
                        [{d.status.toUpperCase()}] Run {d.id.slice(0, 8)} · {d.commit_message?.slice(0, 40) || 'Manual Trigger'}
                      </option>
                    ))
                  )}
                </select>
              </div>
            )}

            {logSource === 'container' && (
              <div>
                <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5 flex items-center gap-1">
                  <Container size={12} className="text-accent" /> Container
                </label>
                <select
                  value={activeService}
                  onChange={(e) => { setActiveService(e.target.value); setLogs([]); }}
                  className="w-full px-3.5 py-2 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all font-mono"
                >
                  {services.length === 0 ? (
                    <option value="">No containers detected</option>
                  ) : (
                    services.map((svc) => (
                      <option key={svc} value={svc}>
                        {svc} ({containerStates[svc]})
                      </option>
                    ))
                  )}
                </select>
              </div>
            )}

            {logSource === 'file' && (
              <div>
                <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5 flex items-center gap-1">
                  <FileText size={12} className="text-accent" /> Log File
                </label>
                <select
                  value={activeLogFile}
                  onChange={(e) => { setActiveLogFile(e.target.value); setLogs([]); }}
                  className="w-full px-3.5 py-2 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all font-mono"
                >
                  {logFiles.length === 0 ? (
                    <option value="">No log files found</option>
                  ) : (
                    logFiles.map((f) => (
                      <option key={f.name} value={f.name}>
                        {f.name} ({(Number(f.size) / 1024).toFixed(1)} KB)
                      </option>
                    ))
                  )}
                </select>
              </div>
            )}

            {logSource === 'container-file' && (
              <div>
                <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5 flex items-center gap-1">
                  <FileText size={12} className="text-accent" /> Container Log File
                </label>
                <select
                  value={activeContainerFileService}
                  onChange={(e) => { setActiveContainerFileService(e.target.value); setLogs([]); }}
                  className="w-full px-3.5 py-2 rounded-xl bg-surface-2/65 border border-line text-ink text-xs focus:outline-none focus:border-accent/40 focus:bg-surface-2 transition-all font-mono"
                >
                  {containerLogFiles.length === 0 ? (
                    <option value="">No container log files found</option>
                  ) : (
                    containerLogFiles.map((f) => (
                      <option key={f.service} value={f.service}>
                        {f.service} ({(Number(f.size) / 1024).toFixed(1)} KB)
                      </option>
                    ))
                  )}
                </select>
              </div>
            )}
          </div>
          
          <div className="flex items-center gap-2 select-none shrink-0 self-stretch md:self-auto justify-end">
            <div className={`px-3 py-1.5 rounded-xl border flex items-center gap-1.5 text-xs font-bold transition-all ${
              isStreaming 
                ? 'bg-good/10 text-good border-good/20 glow-good' 
                : 'bg-surface-2 text-ink-soft border-line'
            }`}>
              {isStreaming ? (
                <>
                  <Wifi size={13} className="animate-pulse text-good" />
                  <span>STREAMING</span>
                </>
              ) : (
                <>
                  <WifiOff size={13} className="text-muted" />
                  <span>HISTORICAL</span>
                </>
              )}
            </div>

            <button 
              onClick={handleDownload} 
              disabled={logs.length === 0}
              className="px-4 py-1.5 rounded-xl bg-surface-2 hover:bg-surface-3 border border-line text-xs font-bold text-ink-soft hover:text-ink disabled:opacity-30 transition-all flex items-center gap-1 shadow-sm h-[32px]"
            >
              <Download size={13} /> Export
            </button>

            <button 
              onClick={handleCopy} 
              disabled={logs.length === 0}
              className="px-4 py-1.5 rounded-xl bg-surface-2 hover:bg-surface-3 border border-line text-xs font-bold text-ink-soft hover:text-ink disabled:opacity-30 transition-all flex items-center gap-1 shadow-sm h-[32px]"
            >
              {copied ? <Check size={13} className="text-good" /> : <Copy size={13} />}
              {copied ? 'Copied' : 'Copy'}
            </button>

            {logSource === 'deployment' && !isStreaming && effectiveDeployId && (
              <button 
                onClick={reconnect} 
                className="px-4 py-1.5 rounded-xl bg-accent/15 text-accent border border-accent/25 hover:bg-accent/25 text-xs font-bold uppercase tracking-wider transition-all h-[32px]"
              >
                Stream
              </button>
            )}
          </div>
        </div>
      </div>

      {/* Log Output Console */}
      <div className="glass-panel border border-line rounded-2xl overflow-hidden flex-1 flex flex-col min-h-0 shadow-2xl relative">
        {/* Terminal Header Decoration */}
        <div className="px-4 py-2 bg-bg border-b border-line/45 flex items-center justify-between select-none">
          <div className="flex items-center gap-1.5 pointer-events-none">
            <div className="w-2.5 h-2.5 rounded-full bg-bad/60"></div>
            <div className="w-2.5 h-2.5 rounded-full bg-warn/60"></div>
            <div className="w-2.5 h-2.5 rounded-full bg-good/60"></div>
          </div>
          <span className="text-[10px] font-mono text-muted tracking-widest uppercase">
            {logSource === 'deployment' ? 'deployment_log' : logSource === 'container' ? `container_log:${activeService}` : logSource === 'container-file' ? `container_file:${activeContainerFileService}` : `file_log:${activeLogFile}`}
          </span>
          <div className="w-10"></div>
        </div>

        <div className="flex items-center gap-2 px-4 py-2 border-b border-line bg-surface-2/40">
          <input
            value={logFilter}
            onChange={e => setLogFilter(e.target.value)}
            placeholder="Filter logs..."
            className="flex-1 px-2.5 py-1 rounded bg-bg border border-line text-ink text-[10px] font-mono focus:outline-none focus:border-accent/40"
          />
          {(['all', 'error', 'warn', 'info'] as const).map(level => (
            <button
              key={level}
              onClick={() => setLogLevel(level)}
              className={`px-2 py-0.5 rounded text-[9px] font-bold uppercase tracking-wider transition-colors ${
                logLevel === level 
                  ? level === 'error' ? 'bg-bad/20 text-bad' : level === 'warn' ? 'bg-warn/20 text-warn' : level === 'info' ? 'bg-info/20 text-info' : 'bg-accent/20 text-accent'
                  : 'text-muted hover:text-ink-soft'
              }`}
            >
              {level}
            </button>
          ))}
          <span className="text-[9px] text-muted font-mono ml-1 shrink-0">
            {filteredLogs.length}/{logs.length}
          </span>
        </div>

        <div className="flex-1 overflow-y-auto p-5 bg-bg font-mono text-[11px] leading-relaxed text-ink-soft">
          {isLoading ? (
            <div className="flex items-center justify-center py-20 gap-2 text-muted font-extrabold uppercase tracking-wider">
              <Loader2 size={14} className="animate-spin text-accent" /> Fetching logs...
            </div>
          ) : logs.length === 0 ? (
            <div className="flex flex-col items-center justify-center py-20 text-muted gap-2">
              <span className="text-[10px] uppercase font-bold tracking-widest">Console Inactive</span>
              <p className="text-xs">
                {logSource === 'deployment' && (isStreaming ? 'Pipe open. Waiting for deployment payload logs...' : 'No historical logs found for the selected release run.')}
                {logSource === 'container' && 'Select a container to view its logs.'}
                {logSource === 'file' && 'Select a log file to view its contents.'}
                {logSource === 'container-file' && 'Select a container log file to view its contents.'}
              </p>
            </div>
          ) : (
            filteredLogs.map((line, i) => {
              const isError = line.toLowerCase().includes('error') || line.toLowerCase().includes('failed') || line.startsWith('[ERROR]');
              const isWarn = line.toLowerCase().includes('warning') || line.startsWith('[WARN]');
              
              let colorClass = 'text-ink-soft/90';
              if (isError) colorClass = 'text-bad font-bold';
              else if (isWarn) colorClass = 'text-warn font-bold';

              return (
                <div 
                  key={i} 
                  className={`whitespace-pre-wrap py-0.5 border-l-2 pl-2.5 mb-0.5 border-transparent hover:bg-surface-3/15 transition-all select-all ${colorClass}`}
                >
                  <span className="text-muted/40 text-[9px] mr-2 select-none">{(i + 1).toString().padStart(4, '0')}</span>
                  {line}
                </div>
              );
            })
          )}
          <div ref={logEndRef} />
        </div>
      </div>
    </div>
  );
}