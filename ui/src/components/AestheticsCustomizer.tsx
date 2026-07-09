import { useState, useEffect } from 'react';
import { Sliders, Palette, Sparkles, Check, RotateCcw, X, Eye } from 'lucide-react';

const themes = [
  { id: 'default', name: 'Obsidian', bg: '#090a0c', surface: '#101216', accent: '#ff6b00', ink: '#f8fafc' },
  { id: 'cyberpunk', name: 'Cyberpunk', bg: '#0f051d', surface: '#1a0b36', accent: '#ff0055', ink: '#00ffcc' },
  { id: 'frost', name: 'Frost', bg: '#eef2f6', surface: '#ffffff', accent: '#0284c7', ink: '#0f172a' },
  { id: 'matrix', name: 'Matrix', bg: '#000000', surface: '#0a0a0a', accent: '#00ff00', ink: '#00ff00' },
  { id: 'dracula', name: 'Dracula', bg: '#282a36', surface: '#1e1f29', accent: '#bd93f9', ink: '#f8f8f2' },
  { id: 'solarized', name: 'Solarized', bg: '#002b36', surface: '#073642', accent: '#b58900', ink: '#fdf6e3' },
  { id: 'monokai', name: 'Monokai', bg: '#272822', surface: '#1e1f1c', accent: '#f92672', ink: '#f8f8f2' },
  { id: 'grayscale', name: 'Grayscale', bg: '#ffffff', surface: '#f9fafb', accent: '#111827', ink: '#111827' },
  { id: 'steel', name: 'Steel', bg: '#1e2530', surface: '#273142', accent: '#ff9f1c', ink: '#eceff4' },
  { id: 'nordic-dark', name: 'NordicDark', bg: '#2e3440', surface: '#3b4252', accent: '#88c0d0', ink: '#eceff4' },
  { id: 'google-material', name: 'Material', bg: '#f8f9fa', surface: '#ffffff', accent: '#1a73e8', ink: '#202124' },
  { id: 'material-high-contrast', name: 'HC Material', bg: '#000000', surface: '#121212', accent: '#80d8ff', ink: '#ffffff' },
];

interface AestheticsCustomizerProps {
  theme: string;
  onThemeChange: (theme: string) => void;
}

export function AestheticsCustomizer({ theme, onThemeChange }: AestheticsCustomizerProps) {
  const [isOpen, setIsOpen] = useState(false);
  const [activeTab, setActiveTab] = useState<'spacing' | 'colors'>('spacing');

  // Spacing & scaling states
  const [paddingScale, setPaddingScale] = useState(() => {
    return parseFloat(localStorage.getItem('custom-padding-scale') || '1.0');
  });
  const [radiusScale, setRadiusScale] = useState(() => {
    return parseFloat(localStorage.getItem('custom-radius-scale') || '1.0');
  });
  const [fontSizeScale, setFontSizeScale] = useState(() => {
    return parseFloat(localStorage.getItem('custom-font-scale') || '1.0');
  });

  // Color states
  const [useCustomAccent, setUseCustomAccent] = useState(() => {
    return localStorage.getItem('custom-accent-enabled') === 'true';
  });
  const [customAccent, setCustomAccent] = useState(() => {
    return localStorage.getItem('custom-accent-color') || '#ff6b00';
  });

  // Apply scales and customizations to :root
  useEffect(() => {
    const root = document.documentElement;

    // Apply Padding Scale
    root.style.setProperty('--padding-card', `${1.25 * paddingScale}rem`);
    root.style.setProperty('--padding-section', `${1.0 * paddingScale}rem`);
    root.style.setProperty('--padding-element', `${0.75 * paddingScale}rem`);
    localStorage.setItem('custom-padding-scale', paddingScale.toString());

    // Apply Radius Scale
    root.style.setProperty('--radius-card', `${12 * radiusScale}px`);
    root.style.setProperty('--radius-btn', `${6 * radiusScale}px`);
    root.style.setProperty('--radius-btn-sm', `${4 * radiusScale}px`);
    localStorage.setItem('custom-radius-scale', radiusScale.toString());

    // Apply Font Size Scale
    root.style.setProperty('--font-size-xs', `${0.75 * fontSizeScale}rem`);
    root.style.setProperty('--font-size-sm', `${0.875 * fontSizeScale}rem`);
    root.style.setProperty('--font-size-base', `${1.0 * fontSizeScale}rem`);
    root.style.setProperty('--font-size-lg', `${1.125 * fontSizeScale}rem`);
    root.style.setProperty('--font-size-xl', `${1.25 * fontSizeScale}rem`);
    localStorage.setItem('custom-font-scale', fontSizeScale.toString());
  }, [paddingScale, radiusScale, fontSizeScale, theme]);

  // Apply Custom Accent Color
  useEffect(() => {
    const root = document.documentElement;
    if (useCustomAccent) {
      root.style.setProperty('--accent', customAccent);
      // Calculate a hover variant (slightly lighter/darker)
      const hoverColor = adjustBrightness(customAccent, 15);
      root.style.setProperty('--accent-hover', hoverColor);
      
      localStorage.setItem('custom-accent-enabled', 'true');
      localStorage.setItem('custom-accent-color', customAccent);
    } else {
      root.style.removeProperty('--accent');
      root.style.removeProperty('--accent-hover');
      localStorage.setItem('custom-accent-enabled', 'false');
    }
  }, [useCustomAccent, customAccent]);

  // Helper to adjust color brightness for hover state
  const adjustBrightness = (hex: string, percent: number) => {
    let R = parseInt(hex.substring(1, 3), 16);
    let G = parseInt(hex.substring(3, 5), 16);
    let B = parseInt(hex.substring(5, 7), 16);

    R = Math.min(255, Math.max(0, R + percent));
    G = Math.min(255, Math.max(0, G + percent));
    B = Math.min(255, Math.max(0, B + percent));

    const rHex = R.toString(16).padStart(2, '0');
    const gHex = G.toString(16).padStart(2, '0');
    const bHex = B.toString(16).padStart(2, '0');

    return `#${rHex}${gHex}${bHex}`;
  };

  const handleReset = () => {
    setPaddingScale(1.0);
    setRadiusScale(1.0);
    setFontSizeScale(1.0);
    setUseCustomAccent(false);
  };

  return (
    <>
      {/* Floating Gear Button */}
      <button
        onClick={() => setIsOpen(!isOpen)}
        className="fixed bottom-6 right-6 w-12 h-12 rounded-full bg-accent hover:bg-accent-hover text-accent-on flex items-center justify-center shadow-lg transition-transform hover:scale-110 active:scale-95 z-50 cursor-pointer"
        title="UI Customizer Panel"
      >
        <Sparkles size={20} className={`${isOpen ? 'rotate-90' : 'animate-pulse'}`} />
      </button>

      {/* Floating Settings Console Drawer */}
      <div
        className={`fixed bottom-20 right-6 w-80 bg-surface/90 border border-line rounded-2xl shadow-[0_20px_50px_rgba(0,0,0,0.3)] backdrop-blur-xl z-50 transition-all duration-300 transform select-none ${
          isOpen ? 'opacity-100 translate-y-0 scale-100' : 'opacity-0 translate-y-8 scale-95 pointer-events-none'
        }`}
      >
        {/* Header */}
        <div className="flex items-center justify-between p-4 border-b border-line">
          <div className="flex items-center gap-2">
            <Sparkles size={14} className="text-accent" />
            <h3 className="text-xs font-bold uppercase tracking-wider text-ink">DevOps UI Customizer</h3>
          </div>
          <div className="flex items-center gap-1">
            <button
              onClick={handleReset}
              className="p-1 rounded hover:bg-surface-2 text-muted hover:text-ink transition-colors cursor-pointer"
              title="Reset Settings"
            >
              <RotateCcw size={13} />
            </button>
            <button
              onClick={() => setIsOpen(false)}
              className="p-1 rounded hover:bg-surface-2 text-muted hover:text-ink transition-colors cursor-pointer"
            >
              <X size={13} />
            </button>
          </div>
        </div>

        {/* Tab Controls */}
        <div className="flex border-b border-line text-[10px] uppercase font-bold tracking-widest bg-surface-2/45">
          <button
            onClick={() => setActiveTab('spacing')}
            className={`flex-1 py-2.5 text-center flex items-center justify-center gap-1.5 border-b-2 cursor-pointer ${
              activeTab === 'spacing'
                ? 'border-accent text-accent bg-surface/50'
                : 'border-transparent text-muted hover:text-ink'
            }`}
          >
            <Sliders size={11} />
            Scale & Layout
          </button>
          <button
            onClick={() => setActiveTab('colors')}
            className={`flex-1 py-2.5 text-center flex items-center justify-center gap-1.5 border-b-2 cursor-pointer ${
              activeTab === 'colors'
                ? 'border-accent text-accent bg-surface/50'
                : 'border-transparent text-muted hover:text-ink'
            }`}
          >
            <Palette size={11} />
            Theme Colors
          </button>
        </div>

        {/* Customizer Parameters Panel */}
        <div className="p-4 space-y-4 max-h-[350px] overflow-y-auto">
          {activeTab === 'spacing' && (
            <>
              {/* Spacing Card */}
              <div className="space-y-4">
                {/* Padding Scale */}
                <div>
                  <div className="flex justify-between items-center mb-1.5">
                    <label className="text-[10px] text-ink-soft uppercase tracking-wider font-extrabold">Padding Scaling</label>
                    <span className="text-[10px] font-mono text-accent font-bold">{(paddingScale * 100).toFixed(0)}%</span>
                  </div>
                  <input
                    type="range"
                    min="0.6"
                    max="1.5"
                    step="0.05"
                    value={paddingScale}
                    onChange={(e) => setPaddingScale(parseFloat(e.target.value))}
                    className="w-full h-1.5 bg-bg rounded-lg appearance-none cursor-pointer accent-accent"
                  />
                  <div className="flex justify-between text-[8px] text-muted font-mono uppercase mt-1">
                    <span>Compact</span>
                    <span>Standard</span>
                    <span>Comfortable</span>
                  </div>
                </div>

                {/* Border Radius Scale */}
                <div>
                  <div className="flex justify-between items-center mb-1.5">
                    <label className="text-[10px] text-ink-soft uppercase tracking-wider font-extrabold">Border Radius Scale</label>
                    <span className="text-[10px] font-mono text-accent font-bold">{(radiusScale * 100).toFixed(0)}%</span>
                  </div>
                  <input
                    type="range"
                    min="0"
                    max="2.5"
                    step="0.1"
                    value={radiusScale}
                    onChange={(e) => setRadiusScale(parseFloat(e.target.value))}
                    className="w-full h-1.5 bg-bg rounded-lg appearance-none cursor-pointer accent-accent"
                  />
                  <div className="flex justify-between text-[8px] text-muted font-mono uppercase mt-1">
                    <span>Sharp (0px)</span>
                    <span>Standard</span>
                    <span>Organic (Pill)</span>
                  </div>
                </div>

                {/* Font Scaling */}
                <div>
                  <div className="flex justify-between items-center mb-1.5">
                    <label className="text-[10px] text-ink-soft uppercase tracking-wider font-extrabold">Font Scaling</label>
                    <span className="text-[10px] font-mono text-accent font-bold">{(fontSizeScale * 100).toFixed(0)}%</span>
                  </div>
                  <input
                    type="range"
                    min="0.8"
                    max="1.3"
                    step="0.02"
                    value={fontSizeScale}
                    onChange={(e) => setFontSizeScale(parseFloat(e.target.value))}
                    className="w-full h-1.5 bg-bg rounded-lg appearance-none cursor-pointer accent-accent"
                  />
                  <div className="flex justify-between text-[8px] text-muted font-mono uppercase mt-1">
                    <span>Dense</span>
                    <span>Default</span>
                    <span>Large</span>
                  </div>
                </div>
              </div>
            </>
          )}

          {activeTab === 'colors' && (
            <>
              <div className="space-y-4">
                {/* Theme Preview Thumbnails */}
                <div>
                  <label className="block text-[10px] text-ink-soft uppercase tracking-wider font-extrabold mb-1.5">Base Theme Preset</label>
                  <div className="grid grid-cols-2 xs:grid-cols-3 gap-2">
                    {themes.map(t => (
                      <button
                        key={t.id}
                        onClick={() => onThemeChange(t.id)}
                        className={`flex items-center gap-2 p-2 rounded-xl border transition-all hover:scale-[1.02] active:scale-[0.98] ${
                          theme === t.id ? 'border-accent ring-1 ring-accent/30' : 'border-line hover:border-line-strong'
                        }`}
                        title={t.name}
                      >
                        <div className="flex gap-0.5 shrink-0">
                          <div className="w-3 h-3 rounded-sm" style={{ background: t.bg }} />
                          <div className="w-3 h-3 rounded-sm" style={{ background: t.surface }} />
                          <div className="w-3 h-3 rounded-sm" style={{ background: t.accent }} />
                          <div className="w-3 h-3 rounded-sm" style={{ background: t.ink }} />
                        </div>
                        <span className="text-[9px] font-bold text-ink-soft truncate">{t.name}</span>
                      </button>
                    ))}
                  </div>
                </div>

                {/* Live Custom Accent Color Picker */}
                <div className="border-t border-line/45 pt-3.5">
                  <div className="flex justify-between items-center mb-2.5">
                    <label className="text-[10px] text-ink-soft uppercase tracking-wider font-extrabold">Custom Accent Override</label>
                    <input
                      type="checkbox"
                      checked={useCustomAccent}
                      onChange={(e) => setUseCustomAccent(e.target.checked)}
                      className="rounded bg-bg border-line text-accent accent-accent w-4 h-4 cursor-pointer"
                    />
                  </div>
                  
                  {useCustomAccent && (
                    <div className="space-y-3">
                      <div className="flex gap-2">
                        <input
                          type="color"
                          value={customAccent}
                          onChange={(e) => setCustomAccent(e.target.value)}
                          className="w-10 h-10 border-0 rounded-xl bg-surface-2 p-0.5 cursor-pointer"
                        />
                        <input
                          type="text"
                          value={customAccent.toUpperCase()}
                          onChange={(e) => {
                            if (e.target.value.startsWith('#') && e.target.value.length <= 7) {
                              setCustomAccent(e.target.value);
                            }
                          }}
                          className="flex-1 px-3.5 py-2.5 rounded-xl bg-bg border border-line text-ink text-xs font-mono focus:outline-none uppercase"
                        />
                      </div>

                      {/* Swatches presets */}
                      <div className="flex flex-wrap gap-1.5">
                        {['#ff6b00', '#3b82f6', '#10b981', '#ef4444', '#bd93f9', '#a6e22e', '#ff0055', '#00ffff'].map((color) => (
                          <button
                            key={color}
                            onClick={() => setCustomAccent(color)}
                            className="w-6 h-6 rounded-full border border-line flex items-center justify-center relative cursor-pointer hover:scale-105 active:scale-95 transition-transform"
                            style={{ backgroundColor: color }}
                          >
                            {customAccent === color && <Check size={11} className="text-black stroke-[3]" />}
                          </button>
                        ))}
                      </div>
                    </div>
                  )}
                </div>
              </div>
            </>
          )}
        </div>

        {/* Live CSS Sandbox Preview badge */}
        <div className="p-3 bg-surface-2/45 border-t border-line text-[9px] font-mono text-muted text-center uppercase tracking-wider flex items-center justify-center gap-1 rounded-b-2xl">
          <Eye size={10} className="text-accent" /> Customizations injected directly to :root
        </div>
      </div>
    </>
  );
}
