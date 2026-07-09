# DevOps Control Plane — Design System (Redesigned)

## Design Identity
A high-fidelity operational workspace blending modern developer portal aesthetics with structured project dashboards. Inspired by Monday.dev grouped table boards and premium dark UI kits, this system uses glowing neon accent borders, double-sidebar navigation, and glassmorphic panels.

## Layout & Structure
- **Left Navigation Strip (64px)**: Minimal, dark-grey control bar with application logo, quick automation toggles, alerts badge, and user profiling.
- **Project Sidebar (256px)**: Glassmorphic dropdown workspace list with real-time project search input, active indicators, and glowing gradient action buttons.
- **Top Header Bar**: Breadcrumb-assisted area with online/offline pulsing status lights, release coordinates, configuration settings, and horizontal scrollable board tabs.
- **Main Content Workspace**: Double-glowing background panel with tab views (Dashboard, Containers, Logs, Environment overrides, Backups, and Danger triggers).

## Theme Tokens & Colors
All visual elements are configured in [index.css](file:///home/sauraku/Documents/sauraku-devops/ui/src/index.css):

### Dark Workspace Backgrounds
- `--color-bg`: `#080a10` (Deep obsidian navy space)
- `--color-surface`: `rgba(15, 18, 29, 0.65)` (Glassmorphism backdrop)
- `--color-surface-2`: `#181d2e` (Card backgrounds)
- `--color-surface-3`: `#222940` (Selected list items)

### Typography
- `--color-ink`: `#f1f5f9` (Bright primary data text)
- `--color-ink-soft`: `#94a3b8` (Secondary information labels)
- `--color-muted`: `#475569` (Inactive placeholders)
- `--color-line`: `rgba(255, 255, 255, 0.08)` (Subtle glass separators)
- `--color-line-strong`: `rgba(255, 255, 255, 0.16)` (Strong action boundaries)

### Glowing Brand Accents
- `--color-accent`: `#00f2fe` (Neon cyan primary indicator)
- `--color-accent-hover`: `#4facfe` (Teal-blue glow transition)
- `--color-purple-accent`: `#7f5af0` (Automation accent)
- `--color-pink-accent`: `#ff007f` (Alert notifications)

### Monday.dev Status Cell Mapping
Rows are formatted into collapsible board groups. Each state is colored using Monday status cards:
- **Done / Active / Success**: Green background (`#00c9a7`) representing fully online runner instances or success builds.
- **Working / Paused / In-Progress**: Orange background (`#ffb515`) representing busy runners or warning buffers.
- **Stuck / Stopped / Unhealthy**: Red background (`#ff4757`) representing aborted/critical pipeline failures.
- **Info / Unverified**: Blue background (`#3b82f6`) representing pending items.

## Typography Configuration
- **Outfit Font**: Employs clean geometry for primary dashboard panels, breadcrumbs, titles, and headers.
- **Inter Font**: Default fallback system sans-serif.
- **JetBrains Mono**: Used for environment variables, Git SHA identifiers, database log offsets, and container mappings.

## Animations & Transitions
- **Pulsing Glows**: Active systems display glowing radial breathings.
- **Glassmorphic Hover Effects**: Elevated containers scale slightly (`scale-[1.02]`), transition border colors, and project drop shadows upon interaction.
