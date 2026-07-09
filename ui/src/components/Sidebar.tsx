import { useState, useRef, useEffect } from 'react';
import type { Project } from '../types';
import { Plus, Search, Folder, Terminal, Cpu, Palette, LogOut } from 'lucide-react';

interface SidebarProps {
  projects: Project[];
  selectedId: string;
  onSelect: (id: string) => void;
  onAddProject: () => void;
  theme: string;
  onThemeChange: (theme: string) => void;
}

export function Sidebar({ projects, selectedId, onSelect, onAddProject, theme, onThemeChange }: SidebarProps) {
  const [searchQuery, setSearchQuery] = useState('');

  const filteredProjects = projects.filter((p) =>
    (p.name || p.id).toLowerCase().includes(searchQuery.toLowerCase())
  );

  const selectedRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    selectedRef.current?.scrollIntoView({ behavior: 'smooth', block: 'nearest', inline: 'center' });
  }, [selectedId]);

  return (
    <aside className="w-full md:w-64 bg-surface border-b md:border-b-0 md:border-r border-line flex flex-col md:flex-col h-auto md:h-full shrink-0 select-none overflow-hidden">
      {/* Top Header Row on Mobile / Left Column Branding on Desktop */}
      <div className="flex flex-row md:flex-col w-full md:w-auto items-center md:items-stretch justify-between md:justify-start border-b border-line bg-surface-2/40 px-4 py-2.5 md:py-4 gap-3">
        <div className="flex items-center gap-2.5">
          <div className="w-7 h-7 md:w-8 md:h-8 rounded-md bg-accent flex items-center justify-center text-accent-on font-black shadow-sm shrink-0">
            <Terminal size={14} className="md:hidden stroke-[3]" />
            <Terminal size={16} className="hidden md:block stroke-[3]" />
          </div>
          <div>
            <p className="text-[8px] md:text-[9px] uppercase tracking-[0.2em] text-muted font-bold leading-none">Control Plane</p>
            <h1 className="text-[10px] md:text-xs font-bold text-ink mt-0.5 md:mt-1 tracking-tight">DEVOPS RUNNER</h1>
          </div>
        </div>

        {/* Mobile quick actions (add project) */}
        <button
          onClick={onAddProject}
          className="md:hidden p-1.5 rounded-xl bg-accent text-accent-on hover:bg-accent-hover transition-colors cursor-pointer"
          title="Register Node"
        >
          <Plus size={14} className="stroke-[3]" />
        </button>
      </div>

      {/* Workspace Panel - Desktop Only */}
      <div className="hidden md:flex px-4 py-3 border-b border-line bg-surface-2/20 items-center justify-between">
        <div className="flex items-center gap-2">
          <Folder size={13} className="text-accent" />
          <span className="text-[11px] font-bold text-ink uppercase tracking-wide">Main Workspace</span>
        </div>
        <span className="text-[9px] font-mono text-muted border border-line px-1 rounded">SYS_V2</span>
      </div>

      {/* Filter / Search Panel - Desktop Only */}
      <div className="hidden md:block p-3 border-b border-line bg-surface-2/10">
        <div className="relative flex items-center">
          <Search size={12} className="absolute left-2.5 text-muted" />
          <input
            type="text"
            placeholder="FILTER PROJECTS..."
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            className="w-full pl-7 pr-3 py-1.5 rounded bg-bg border border-line text-ink text-[10px] focus:outline-none focus:border-accent/80 transition-colors placeholder-muted font-mono uppercase tracking-wider"
          />
        </div>
      </div>

      {/* Project Items list - Horizontal Scroll on Mobile, Vertical List on Desktop */}
      <div className="flex-1 overflow-x-auto md:overflow-y-auto flex flex-row md:flex-col p-2 gap-1.5 md:gap-0.5 items-center md:items-stretch">
        {/* Label - Desktop Only */}
        <div className="hidden md:flex px-2 py-1 items-center justify-between">
          <span className="text-[9px] uppercase tracking-[0.15em] text-muted font-bold">
            Registered Nodes ({filteredProjects.length})
          </span>
        </div>

        {filteredProjects.length === 0 ? (
          <div className="px-3 py-2 md:py-6 text-center border border-dashed border-line rounded bg-surface-2/10 whitespace-nowrap md:whitespace-normal shrink-0">
            <p className="text-[9px] md:text-[10px] text-muted font-mono uppercase">Node list empty</p>
          </div>
        ) : (
          filteredProjects.map((p) => {
            const isSelected = selectedId === p.id;
            return (
              <button
                key={p.id}
                ref={p.id === selectedId ? selectedRef : undefined}
                onClick={() => onSelect(p.id)}
                className={`text-left px-3 py-3 md:py-2 rounded transition-all relative overflow-hidden group shrink-0 ${
                  isSelected
                    ? 'bg-surface-2 border border-line text-ink font-bold'
                    : 'text-ink-soft border border-transparent hover:bg-surface-2/30 hover:text-ink'
                }`}
              >
                {/* Accent line - Left on Desktop, Bottom on Mobile */}
                {isSelected && (
                  <>
                    <div className="hidden md:block absolute left-0 top-0 bottom-0 w-[3px] bg-accent"></div>
                    <div className="md:hidden absolute bottom-0 left-0 right-0 h-[2.5px] bg-accent"></div>
                  </>
                )}

                <div className="flex items-center gap-2 md:gap-2.5 relative z-10">
                  <div
                    className={`p-1 rounded ${
                      isSelected
                        ? 'bg-accent/15 text-accent'
                        : 'bg-bg text-muted group-hover:text-ink-soft'
                    }`}
                  >
                    <Cpu size={11} />
                  </div>
                  <div className="min-w-0 max-w-[120px] md:max-w-none text-left">
                    <div className="flex items-center gap-1.5">
                      <span className={`w-1.5 h-1.5 rounded-full shrink-0 ${
                        p.runner_status === 'active' ? 'bg-good' :
                        p.runner_status === 'starting' ? 'bg-warn animate-pulse' :
                        p.runner_status === 'error' ? 'bg-bad' :
                        'bg-muted'
                      }`} />
                      <div className="font-bold text-[10px] md:text-xs truncate uppercase tracking-tight">{p.name || p.id}</div>
                    </div>
                    <div className="hidden md:block text-[9px] text-muted truncate font-mono mt-0.5 select-all">
                      {p.repo_url ? p.repo_url.replace('https://github.com/', '') : 'no-repo'} · {p.branch_name}
                    </div>
                  </div>
                </div>
              </button>
            );
          })
        )}
      </div>

      {/* Theme Quick Switcher - Desktop Only */}
      <div className="hidden md:flex p-3 border-t border-line bg-surface-2/20 flex-col gap-1.5 font-bold">
        <label className="text-[9px] uppercase tracking-[0.15em] text-muted font-bold flex items-center gap-1.5 select-none">
          <Palette size={11} className="text-accent" /> Active Theme
        </label>
        <select
          value={theme}
          onChange={(e) => onThemeChange(e.target.value)}
          className="w-full px-2.5 py-1.5 rounded bg-bg border border-line text-ink text-[10px] focus:outline-none focus:border-accent transition-colors font-mono uppercase tracking-wider cursor-pointer font-bold"
        >
          <option value="default">Obsidian Black (Default)</option>
          <option value="cyberpunk">Cyberpunk Neon</option>
          <option value="frost">Nordic Frost</option>
          <option value="matrix">Terminal Matrix</option>
          <option value="dracula">Dracula Dracula</option>
          <option value="solarized">Solarized Dark</option>
          <option value="monokai">Monokai Retro</option>
          <option value="grayscale">Grayscale Light</option>
          <option value="steel">Steel Utilitarian</option>
          <option value="nordic-dark">Nordic Dark</option>
          <option value="google-material">Google Material</option>
          <option value="material-high-contrast">Material High Contrast</option>
        </select>
      </div>

      {/* Footer Actions */}
      <div className="p-3 border-t border-line bg-surface-2/40 space-y-2">
        <button
          onClick={() => { window.location.href = '/logout'; }}
          className="w-full py-2 px-3 rounded border border-line text-muted hover:text-ink hover:border-line-strong text-[10px] font-bold uppercase tracking-wider transition-colors flex items-center justify-center gap-1.5 active:translate-y-[0.5px]"
        >
          <LogOut size={12} />
          Sign Out
        </button>
        <button
          onClick={onAddProject}
          className="w-full py-2 px-3 rounded bg-accent text-accent-on font-bold text-xs transition-colors hover:bg-accent-hover flex items-center justify-center gap-1.5 uppercase tracking-wider active:translate-y-[0.5px]"
        >
          <Plus size={12} className="stroke-[3]" />
          Register Node
        </button>
      </div>
    </aside>
  );
}
