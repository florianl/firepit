// The firepit web UI: fetches the views from the API and renders them with
// d3 and d3-flame-graph, which are loaded as classic scripts before this
// module. Everything that does not touch the DOM lives in lib.js.

import {
    buildExport,
    exportFilename,
    formatTime,
    frameColor,
    frameLabel,
    hasData,
    matchesFrameFilter,
    resourceFilterParams,
    sanitizeId,
    sortSandwichFunctions,
    toQuery,
} from './lib.js';

let updateInterval = null;
let isPaused = false;

// Refresh on server events. Poll while the stream is not connected.
const POLL_INTERVAL_MS = 5000;
let eventSource = null;

function startPolling() {
    if (updateInterval === null) {
        updateInterval = setInterval(updateChart, POLL_INTERVAL_MS);
    }
}

function stopPolling() {
    clearInterval(updateInterval);
    updateInterval = null;
}

function startUpdates() {
    stopUpdates();
    if (typeof EventSource === 'undefined') {
        startPolling();
        return;
    }
    startPolling();
    eventSource = new EventSource(window.__BASE_PATH__ + '/api/events');
    // Also sent after a reconnect, so catch up.
    eventSource.addEventListener('ready', () => {
        stopPolling();
        updateChart();
    });
    eventSource.addEventListener('change', () => updateChart());
    // The browser reconnects on its own.
    eventSource.onerror = () => startPolling();
}

function stopUpdates() {
    stopPolling();
    if (eventSource !== null) {
        eventSource.close();
        eventSource = null;
    }
}

let activeTab = null;
let resourceFilters = []; // {key, value}, all must match
let selectedAttrKey = '';
let resourceAttributeMap = new Map(); // key -> values
let lastResourceTypesKey = '';
let frameFilter = '';
let currentGraphs = null;
let viewMode = 'flamegraph';
let currentHeatMaps = null;
let sortMode = 'time-ordered';
// Attribute keys rendered as virtual frames in the flame graph view,
// outermost first. None are selected unless configured on the server.
let virtualFrames = [];
let attributeKeys = [];
let lastAttributeKeysKey = '';

// Rebuilds the filter chips and the selects. Filtered keys are not
// offered again, as a resource has one value per key.
function rebuildResourceFilterControls() {
    const chips = document.getElementById('resource-filter-chips');
    chips.replaceChildren();
    resourceFilters.forEach((f, i) => {
        const chip = document.createElement('span');
        chip.className = 'vframe-chip';
        chip.textContent = f.key + ' = ' + f.value;
        const remove = document.createElement('button');
        remove.type = 'button';
        remove.title = 'Remove filter ' + f.key;
        remove.textContent = '×';
        remove.addEventListener('click', () => {
            resourceFilters = resourceFilters.filter((_, j) => j !== i);
            rebuildResourceFilterControls();
            updateChart(true);
        });
        chip.appendChild(remove);
        chips.appendChild(chip);
    });

    const used = new Set(resourceFilters.map(f => f.key));
    const keys = [...resourceAttributeMap.keys()].filter(k => !used.has(k));
    if (!keys.includes(selectedAttrKey)) selectedAttrKey = '';

    const keySelect = document.getElementById('resource-key-select');
    keySelect.replaceChildren();
    const placeholder = document.createElement('option');
    placeholder.value = '';
    placeholder.textContent = resourceFilters.length ? '+ and' : 'All';
    keySelect.appendChild(placeholder);
    keys.forEach(k => {
        const o = document.createElement('option');
        o.value = k;
        o.textContent = k;
        keySelect.appendChild(o);
    });
    keySelect.value = selectedAttrKey;
    keySelect.disabled = keys.length === 0;

    rebuildValueSelect();
}

function rebuildValueSelect() {
    const valueSelect = document.getElementById('resource-value-select');
    valueSelect.replaceChildren();

    const placeholder = document.createElement('option');
    placeholder.value = '';
    valueSelect.appendChild(placeholder);

    if (!selectedAttrKey) {
        placeholder.textContent = '(select attribute)';
        valueSelect.disabled = true;
        return;
    }

    placeholder.textContent = '(select value)';
    valueSelect.disabled = false;
    (resourceAttributeMap.get(selectedAttrKey) || []).forEach(v => {
        const o = document.createElement('option');
        o.value = v;
        o.textContent = v;
        valueSelect.appendChild(o);
    });
    valueSelect.value = '';
}

function rebuildVirtualFrameControls() {
    const chips = document.getElementById('vframe-chips');
    chips.innerHTML = '';
    virtualFrames.forEach((key, i) => {
        const chip = document.createElement('span');
        chip.className = 'vframe-chip';
        chip.textContent = key;
        const remove = document.createElement('button');
        remove.type = 'button';
        remove.title = 'Remove ' + key;
        remove.textContent = '×';
        remove.addEventListener('click', () => {
            virtualFrames = virtualFrames.filter((_, j) => j !== i);
            rebuildVirtualFrameControls();
            updateChart(true);
        });
        chip.appendChild(remove);
        chips.appendChild(chip);
    });

    const addSelect = document.getElementById('vframe-add-select');
    addSelect.innerHTML = '';
    const placeholder = document.createElement('option');
    placeholder.value = '';
    placeholder.textContent = '+ add';
    addSelect.appendChild(placeholder);
    attributeKeys
        .filter(k => !virtualFrames.includes(k))
        .forEach(k => {
            const o = document.createElement('option');
            o.value = k;
            o.textContent = k;
            addSelect.appendChild(o);
        });
    addSelect.value = '';
    addSelect.disabled = addSelect.options.length <= 1;
}

// Appends the shared query parameters for flame graph based endpoints.
// Virtual frames are only selectable in, and applied to, the flame graph view.
function appendCommonParams(params) {
    resourceFilterParams(resourceFilters).forEach(f => params.append('resourceType', f));
    params.set('virtualFrames', viewMode === 'flamegraph' ? virtualFrames.join(',') : '');
    return params;
}

// messageElement returns a div with the given class and text. Profile
// data is untrusted and must not be parsed as HTML.
function messageElement(className, text) {
    const el = document.createElement('div');
    el.className = className;
    el.textContent = text;
    return el;
}

// errorElement returns a status box showing err.
function errorElement(err) {
    const el = document.createElement('div');
    el.className = 'status-box';
    const label = document.createElement('strong');
    label.textContent = 'Error:';
    el.append(label, ' ' + (err && err.message ? err.message : String(err)));
    return el;
}

function togglePause() {
    const btn = document.getElementById('pause-btn');
    const filterContainer = document.getElementById('filter-container');
    const filterInput = document.getElementById('frame-filter');

    isPaused = !isPaused;
    if (isPaused) {
        stopUpdates();
        btn.textContent = 'Resume';
        if (viewMode === 'flamegraph') {
            filterContainer.classList.add('visible');
            filterInput.focus();
        }
    } else {
        filterContainer.classList.remove('visible');
        frameFilter = '';
        filterInput.value = '';
        updateChart(true);
        startUpdates();
        btn.textContent = 'Pause';
    }
}

function onViewModeChange(selectedMode) {
    viewMode = selectedMode;
    const filterContainer = document.getElementById('filter-container');
    const sortFilter = document.getElementById('sort-mode-filter');
    if (viewMode === 'heatmap' || viewMode === 'sandwich') {
        if (isPaused) filterContainer.classList.remove('visible');
        sortFilter.style.display = 'none';
    } else {
        if (isPaused) filterContainer.classList.add('visible');
        sortFilter.style.display = 'flex';
    }
    document.getElementById('vframe-filter').style.display = viewMode === 'flamegraph' ? 'flex' : 'none';
    updateChart(true);
}

function applyFrameFilter() {
    const filterText = document.getElementById('frame-filter').value.toLowerCase();
    frameFilter = filterText;

    // Get all rect elements (the actual frame rectangles)
    const rects = document.querySelectorAll('svg rect');

    if (!filterText) {
        // No filter: remove all dimming and highlight classes from groups
        document.querySelectorAll('svg g.flame-dimmed, svg g.flame-highlight').forEach(group => {
            group.classList.remove('flame-dimmed', 'flame-highlight');
        });
    } else {
        // Filter applied: dim all frames first (apply classes to parent groups)
        rects.forEach(rect => {
            const parent = rect.parentElement;
            if (parent && parent.tagName === 'g') {
                parent.classList.add('flame-dimmed');
            }
        });

        // Then highlight matching frames
        rects.forEach(rect => {
            // Get frame name from d3 data
            const frameName = rect.__data__ && rect.__data__.data ? rect.__data__.data.name : '';

            if (matchesFrameFilter(frameName, filterText)) {
                const parent = rect.parentElement;
                if (parent && parent.tagName === 'g') {
                    parent.classList.remove('flame-dimmed');
                    parent.classList.add('flame-highlight');
                }
            }
        });
    }
}

function renderFlamegraph(containerId, data) {
    const container = document.getElementById(containerId);
    if (!container) return;
    container.innerHTML = '';

    const width = document.documentElement.clientWidth;
    const height = Math.max(400, window.innerHeight - 220);

    const sortFn = sortMode === 'left-heavy'
        ? (a, b) => b.value - a.value
        : false;

    const fg = flamegraph.default()
        .width(width)
        .height(height)
        .sort(sortFn)
        .setLabelHandler(function(d) {
            const samples = ('v' in d) ? d.v : d.value;
            return frameLabel(d.data.name || d.data.n || '', d.x1 - d.x0, samples, d.data.filename);
        })
        .setColorMapper(function(d, originalColor) {
            // Keep the color the library uses to highlight search results.
            if (originalColor === '#E600E6') return originalColor;
            return frameColor(d.data.frametype, d.data.name) || originalColor;
        });

    // The id is not guaranteed to be a valid CSS selector.
    const selection = d3.select(container);
    selection.datum(data).call(fg);

    // Reapply frame filter if one is active
    if (frameFilter) {
        setTimeout(applyFrameFilter, 0);
    }

    // Listen for clicks on the flamegraph and reapply filter after interaction
    selection.on('click', function() {
        if (frameFilter) {
            setTimeout(applyFrameFilter, 50);
        }
    });
}

function exportCurrentGraph() {
    if (!currentGraphs || currentGraphs.length === 0) {
        alert('No data to export');
        return;
    }

    const activeGraph = currentGraphs.find(g => g.type === activeTab);
    if (!activeGraph) {
        alert('No data to export');
        return;
    }

    const now = new Date();
    const exportData = buildExport(activeGraph, resourceFilterParams(resourceFilters).join(', '), virtualFrames, now);

    const jsonString = JSON.stringify(exportData, null, 2);
    const blob = new Blob([jsonString], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = exportFilename(activeGraph.type, now);
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
}

// URL and ETag the current view was rendered from.
let lastRenderedKey = null;

// fetchViewData resolves to {data}, or to null if the view is already
// rendered from this response and force is not set.
function fetchViewData(endpoint, query, force) {
    const url = window.__BASE_PATH__ + endpoint + query;
    return fetch(url).then(r => {
        const etag = r.ok ? r.headers.get('ETag') : null;
        const key = etag ? url + ' ' + etag : null;
        if (!force && key !== null && key === lastRenderedKey) {
            if (r.body) r.body.cancel().catch(() => {});
            return null;
        }
        return r.json().then(data => {
            lastRenderedKey = key;
            return {data};
        });
    }).catch(err => {
        lastRenderedKey = null;
        throw err;
    });
}

function markUpdated() {
    document.getElementById('last-update').textContent =
        new Date().toLocaleTimeString();
}

// updateChart refreshes the current view. Unless force is set, unchanged
// data is not rendered again, which keeps the zoom state.
function updateChart(force) {
    force = force === true;
    const query = toQuery(appendCommonParams(new URLSearchParams()));

    if (viewMode === 'heatmap') {
        fetchAndRenderHeatMap(query, force);
    } else if (viewMode === 'sandwich') {
        fetchAndRenderSandwich(query, force);
    } else {
        fetchAndRenderFlamegraph(query, force);
    }

    // Profile count / time range
    fetch(window.__BASE_PATH__ + '/api/profiles')
        .then(r => r.json())
        .then(data => {
            if (data) {
                document.getElementById('profile-count').textContent = data.count || 0;
                if (data.timeRange)
                    document.getElementById('time-range').textContent = data.timeRange;
            }
        })
        .catch(() => {});

    // Attribute keys available as virtual frames
    fetch(window.__BASE_PATH__ + '/api/attribute-keys')
        .then(r => r.json())
        .then(keys => {
            const newKeys = keys || [];
            const newKey = JSON.stringify(newKeys);
            if (newKey === lastAttributeKeysKey) return;
            lastAttributeKeysKey = newKey;
            attributeKeys = newKeys;
            rebuildVirtualFrameControls();
        })
        .catch(() => {});

    // Resource attributes for filtering
    fetch(window.__BASE_PATH__ + '/api/resource-attributes')
        .then(r => r.json())
        .then(attrs => {
            const newAttrs = attrs || [];
            const newKey = JSON.stringify(newAttrs);
            if (newKey === lastResourceTypesKey) return;
            lastResourceTypesKey = newKey;

            resourceAttributeMap = new Map(newAttrs.map(a => [a.key, a.values || []]));

            // Drop stale filters as they would hide all profiles
            const before = resourceFilters.length;
            resourceFilters = resourceFilters.filter(f =>
                (resourceAttributeMap.get(f.key) || []).includes(f.value));
            rebuildResourceFilterControls();
            if (resourceFilters.length !== before) updateChart(true);
        })
        .catch(() => {});
}

function fetchAndRenderFlamegraph(query, force) {
    fetchViewData('/api/flamegraph', query, force)
        .then(res => {
            if (res === null) { markUpdated(); return; }
            const graphs = res.data;
            currentGraphs = graphs;
            // graphs: [{type, unit, root}, ...]
            const strip  = document.getElementById('tab-strip');
            const charts = document.getElementById('charts');

            const noData = !graphs || graphs.length === 0 ||
                           graphs.every(g => !hasData(g.root));

            if (noData) {
                strip.style.display  = 'none';
                charts.innerHTML     =
                    '<div class="waiting">Waiting for profiles...</div>';
                return;
            }

            // Default activeTab to first type on first render
            if (activeTab === null || !graphs.find(g => g.type === activeTab)) {
                activeTab = graphs[0].type;
            }

            // Rebuild tab strip
            strip.style.display = 'flex';
            strip.innerHTML = '';
            graphs.forEach(g => {
                const label = g.unit ? `${g.type} (${g.unit})` : g.type;
                const btn   = document.createElement('button');
                btn.className  = 'tab-btn' + (g.type === activeTab ? ' active' : '');
                btn.textContent = label;
                btn.dataset.type = g.type;
                btn.addEventListener('click', () => {
                    activeTab = g.type;
                    document.querySelectorAll('.tab-btn').forEach(
                        b => b.classList.toggle('active', b.dataset.type === activeTab)
                    );
                    document.querySelectorAll('.tab-panel').forEach(
                        p => p.classList.toggle('active', p.id === 'panel-' + sanitizeId(activeTab))
                    );
                });
                strip.appendChild(btn);
            });

            // Rebuild panels
            charts.innerHTML = '';
            graphs.forEach(g => {
                const panel = document.createElement('div');
                const panelId = 'panel-' + sanitizeId(g.type);
                panel.className = 'tab-panel' + (g.type === activeTab ? ' active' : '');
                panel.id = panelId;
                charts.appendChild(panel);

                if (hasData(g.root)) {
                    setTimeout(() => renderFlamegraph(panelId, g.root), 0);
                } else {
                    panel.replaceChildren(messageElement('waiting', 'No data for ' + g.type));
                }
            });

            document.getElementById('last-update').textContent =
                new Date().toLocaleTimeString();
        })
        .catch(err => {
            document.getElementById('charts').replaceChildren(errorElement(err));
        });

    document.getElementById('last-update').textContent =
        new Date().toLocaleTimeString();
}

function fetchAndRenderHeatMap(query, force) {
    fetchViewData('/api/flamescope', query, force)
        .then(res => {
            if (res === null) { markUpdated(); return; }
            const hmaps = res.data;
            currentHeatMaps = hmaps;
            const strip  = document.getElementById('tab-strip');
            const charts = document.getElementById('charts');

            const noData = !hmaps || hmaps.length === 0 ||
                           hmaps.every(h => !h.data || h.data.numSeconds === 0);

            if (noData) {
                strip.style.display = 'none';
                charts.innerHTML = '<div class="waiting">Waiting for profiles...</div>';
                return;
            }

            if (activeTab === null || !hmaps.find(h => h.type === activeTab)) {
                activeTab = hmaps[0].type;
            }

            // Rebuild tab strip
            strip.style.display = 'flex';
            strip.innerHTML = '';
            hmaps.forEach(h => {
                const btn = document.createElement('button');
                btn.className = 'tab-btn' + (h.type === activeTab ? ' active' : '');
                btn.textContent = h.type;
                btn.dataset.type = h.type;
                btn.addEventListener('click', () => {
                    activeTab = h.type;
                    document.querySelectorAll('.tab-btn').forEach(
                        b => b.classList.toggle('active', b.dataset.type === activeTab)
                    );
                    renderHeatMapForType(activeTab);
                });
                strip.appendChild(btn);
            });

            // Render initial heatmap
            charts.innerHTML = '';
            renderHeatMapForType(activeTab);
            document.getElementById('last-update').textContent =
                new Date().toLocaleTimeString();
        })
        .catch(err => {
            document.getElementById('charts').replaceChildren(errorElement(err));
        });
}

function renderHeatMapForType(type) {
    const charts = document.getElementById('charts');
    const entry = (currentHeatMaps || []).find(h => h.type === type);
    if (!entry || !entry.data || entry.data.numSeconds === 0) {
        charts.replaceChildren(messageElement('waiting', 'No heatmap data for ' + type));
        return;
    }
    renderHeatMap(charts, entry.data);
}

function renderHeatMap(container, d) {
    const marginTop    = 30;
    const marginRight  = 20;
    const marginBottom = 60;
    const marginLeft   = 60;

    const totalWidth   = document.documentElement.clientWidth;
    const totalHeight  = Math.max(420, window.innerHeight - 240);
    const innerWidth   = totalWidth  - marginLeft - marginRight;
    const innerHeight  = totalHeight - marginTop  - marginBottom;

    // Scales
    const xScale = d3.scaleLinear()
        .domain([0, d.numSeconds])
        .range([0, innerWidth]);

    const yScale = d3.scaleLinear()
        .domain([0, d.numSubBuckets])
        .range([0, innerHeight]);

    const cellW = innerWidth  / d.numSeconds;
    const cellH = innerHeight / d.numSubBuckets;

    // Color scale: white to dark red (similar to opentelemetry-ebpf-profiler)
    const color = d3.scaleSequential(d3.interpolateRgb('#ffffff', '#8b0000'))
        .domain([0, d.maxValue || 1]);

    container.innerHTML = '';
    const svg = d3.select(container).append('svg')
        .attr('width',  totalWidth)
        .attr('height', totalHeight);

    const g = svg.append('g')
        .attr('transform', `translate(${marginLeft},${marginTop})`);

    // Build cell data (only non-zero cells)
    const cellData = [];
    for (let xi = 0; xi < d.numSeconds; xi++) {
        for (let yi = 0; yi < d.numSubBuckets; yi++) {
            const v = d.cells[xi * d.numSubBuckets + yi];
            if (v > 0) cellData.push({ xi, yi, v });
        }
    }

    const tooltip = document.getElementById('heatmap-tooltip');

    // Render cells
    g.selectAll('rect.hm-cell')
        .data(cellData)
        .join('rect')
        .attr('class', 'hm-cell')
        .attr('x', c => xScale(c.xi))
        .attr('y', c => yScale(c.yi))
        .attr('width',  Math.max(cellW, 1))
        .attr('height', Math.max(cellH, 1))
        .attr('fill', c => color(c.v))
        .on('mousemove', function(event, c) {
            const offsetMs = c.yi * d.subBucketMs;
            const wallSec  = d.minTimeSec + c.xi;
            const wallTime = new Date(wallSec * 1000).toLocaleTimeString();
            tooltip.style.display  = 'block';
            tooltip.style.left     = (event.clientX + 12) + 'px';
            tooltip.style.top      = (event.clientY - 28) + 'px';
            tooltip.textContent    =
                `t=${wallTime} +${offsetMs}ms  samples=${c.v}`;
        })
        .on('mouseleave', () => { tooltip.style.display = 'none'; });

    // X-axis labels (wall-clock time)
    const tickStep = Math.max(1, Math.round(d.numSeconds / 10));
    const xTicks = d3.range(0, d.numSeconds, tickStep);

    const xAxis = g.append('g')
        .attr('transform', `translate(0, ${innerHeight})`);
    xTicks.forEach(xi => {
        const wallSec = d.minTimeSec + xi;
        const label   = new Date(wallSec * 1000).toLocaleTimeString();
        xAxis.append('text')
            .attr('x', xScale(xi))
            .attr('y', 20)
            .attr('font-size', '11px')
            .attr('fill', '#555')
            .attr('text-anchor', 'middle')
            .text(label);
        xAxis.append('line')
            .attr('x1', xScale(xi)).attr('x2', xScale(xi))
            .attr('y1', 0).attr('y2', 5)
            .attr('stroke', '#aaa');
    });
    xAxis.append('text')
        .attr('x', innerWidth / 2).attr('y', 50)
        .attr('text-anchor', 'middle')
        .attr('font-size', '12px').attr('fill', '#333')
        .text('Time');

    // Y-axis labels (sub-second offset)
    const yTickCount = 10;
    const yAxis = g.append('g');
    d3.range(0, d.numSubBuckets + 1, d.numSubBuckets / yTickCount).forEach(yi => {
        const ms = Math.round(yi * d.subBucketMs);
        yAxis.append('text')
            .attr('x', -8).attr('y', yScale(yi) + cellH / 2)
            .attr('text-anchor', 'end')
            .attr('dominant-baseline', 'middle')
            .attr('font-size', '11px').attr('fill', '#555')
            .text(ms + 'ms');
        yAxis.append('line')
            .attr('x1', -4).attr('x2', 0)
            .attr('y1', yScale(yi)).attr('y2', yScale(yi))
            .attr('stroke', '#aaa');
    });
    yAxis.append('text')
        .attr('transform', `rotate(-90)`)
        .attr('x', -(innerHeight / 2)).attr('y', -45)
        .attr('text-anchor', 'middle')
        .attr('font-size', '12px').attr('fill', '#333')
        .text('Sub-second offset');

    // Color legend (white to dark red)
    const legendW = 120, legendH = 10;
    const legendX = innerWidth - legendW;
    const legendY = -20;
    const defs  = svg.append('defs');
    const grad  = defs.append('linearGradient').attr('id', 'hm-legend-grad')
        .attr('x1', '0%').attr('y1', '0%')
        .attr('x2', '100%').attr('y2', '0%');
    grad.append('stop').attr('offset', '0%').attr('stop-color', '#ffffff');
    grad.append('stop').attr('offset', '50%').attr('stop-color', '#ff6b6b');
    grad.append('stop').attr('offset', '100%').attr('stop-color', '#8b0000');
    g.append('rect')
        .attr('x', legendX).attr('y', legendY)
        .attr('width', legendW).attr('height', legendH)
        .attr('fill', 'url(#hm-legend-grad)');
    g.append('text').attr('x', legendX).attr('y', legendY - 2)
        .attr('font-size', '10px').attr('fill', '#555').text('0');
    g.append('text').attr('x', legendX + legendW).attr('y', legendY - 2)
        .attr('text-anchor', 'end')
        .attr('font-size', '10px').attr('fill', '#555').text(d.maxValue);
}

let sandwichSortColumn = 'total';
let sandwichSortAsc = false;
let selectedFunctionName = null;

function fetchAndRenderSandwich(query, force) {
    fetchViewData('/api/sandwich', query, force)
        .then(res => {
            if (res === null) { markUpdated(); return; }
            const sandwiches = res.data;
            currentGraphs = sandwiches;
            const strip  = document.getElementById('tab-strip');
            const charts = document.getElementById('charts');

            const noData = !sandwiches || sandwiches.length === 0 ||
                           sandwiches.every(s => !s.data || !s.data.functions || s.data.functions.length === 0);

            if (noData) {
                strip.style.display  = 'none';
                charts.innerHTML     = '<div class="waiting">Waiting for profiles...</div>';
                return;
            }

            if (activeTab === null || !sandwiches.find(s => s.type === activeTab)) {
                activeTab = sandwiches[0].type;
            }

            strip.style.display = 'flex';
            strip.innerHTML = '';
            sandwiches.forEach(s => {
                const btn   = document.createElement('button');
                btn.className  = 'tab-btn' + (s.type === activeTab ? ' active' : '');
                btn.textContent = s.type;
                btn.dataset.type = s.type;
                btn.addEventListener('click', () => {
                    activeTab = s.type;
                    selectedFunctionName = null;
                    document.querySelectorAll('.tab-btn').forEach(
                        b => b.classList.toggle('active', b.dataset.type === activeTab)
                    );
                    document.querySelectorAll('.tab-panel').forEach(
                        p => p.classList.toggle('active', p.id === 'panel-' + sanitizeId(activeTab))
                    );
                });
                strip.appendChild(btn);
            });

            // Graph mode controls removed - using single DAG view instead

            charts.innerHTML = '';
            sandwiches.forEach(s => {
                const panel = document.createElement('div');
                const panelId = 'panel-' + sanitizeId(s.type);
                panel.className = 'tab-panel sandwich-container' + (s.type === activeTab ? ' active' : '');
                panel.id = panelId;
                charts.appendChild(panel);

                if (s.data && s.data.functions && s.data.functions.length > 0) {
                    setTimeout(() => renderSandwichView(panelId, s.data), 0);
                } else {
                    panel.replaceChildren(messageElement('waiting', 'No data for ' + s.type));
                }
            });

            document.getElementById('last-update').textContent =
                new Date().toLocaleTimeString();
        })
        .catch(err => {
            document.getElementById('charts').replaceChildren(errorElement(err));
        });
}

function renderSandwichView(containerId, data) {
    selectedFunctionName = null;
    const container = document.getElementById(containerId);
    if (!container) return;

    container.innerHTML = '';

    const leftPanel = document.createElement('div');
    leftPanel.className = 'sandwich-left';
    leftPanel.id = 'sandwich-left-' + containerId;

    const divider = document.createElement('div');
    divider.className = 'sandwich-divider';
    divider.id = 'sandwich-divider-' + containerId;

    const rightPanel = document.createElement('div');
    rightPanel.className = 'sandwich-right';
    rightPanel.id = 'sandwich-right-' + containerId;

    container.appendChild(leftPanel);
    container.appendChild(divider);
    container.appendChild(rightPanel);

    renderSandwichTable(leftPanel, data);
    renderSandwichGraphPlaceholder(rightPanel);

    setupSandwichDivider(divider, leftPanel);
}

function renderSandwichGraphPlaceholder(container) {
    container.innerHTML = '<div class="waiting">Please select a function.</div>';
}

function fetchSandwichDetail(functionName, sampleType, resourceTypes, rightPanel) {
    const params = new URLSearchParams({ functionName: functionName, sampleType: sampleType });
    resourceTypes.forEach(f => params.append('resourceType', f));
    params.set('virtualFrames', '');
    const query = toQuery(params);

    fetch(window.__BASE_PATH__ + '/api/sandwich-detail' + query)
        .then(r => r.json())
        .then(data => {
            if (data && data[sampleType] && data[sampleType].callers && data[sampleType].callees) {
                renderSandwichGraphs(rightPanel, data[sampleType]);
            } else {
                rightPanel.innerHTML = '<div class="waiting">No data for this function.</div>';
            }
        })
        .catch(err => {
            rightPanel.replaceChildren(errorElement(err));
        });
}

function renderSandwichGraphs(container, sandwichData) {
    container.innerHTML = '';
    window.currentSandwichData = sandwichData;

    // Create DAG section (only one now, replacing separate callers/callees)
    const dagSection = document.createElement('div');
    dagSection.className = 'sandwich-graph';
    const dagContainer = document.createElement('div');
    dagContainer.id = 'dag-graph';
    dagContainer.style.height = '100%';
    dagSection.appendChild(dagContainer);

    container.appendChild(dagSection);

    // Render DAG (shown by default)
    renderDAGGraph(dagContainer, sandwichData);
}

function renderSandwichTable(container, data) {
    if (!data || !data.functions) return;

    // Clear container first to ensure only one table is rendered
    container.innerHTML = '';

    const functions = [...data.functions];
    sortSandwichFunctions(functions, sandwichSortColumn, sandwichSortAsc);

    const table = document.createElement('table');
    table.className = 'sandwich-table';

    const thead = document.createElement('thead');
    const headerRow = document.createElement('tr');

    const createHeader = (text, sortKey) => {
        const th = document.createElement('th');
        th.className = 'sortable';
        th.textContent = text;
        if (sortKey === sandwichSortColumn) {
            th.classList.add(sandwichSortAsc ? 'sorted-asc' : 'sorted-desc');
        }
        th.addEventListener('click', () => {
            if (sandwichSortColumn === sortKey) {
                sandwichSortAsc = !sandwichSortAsc;
            } else {
                sandwichSortColumn = sortKey;
                sandwichSortAsc = false;
            }
            renderSandwichTable(container, data);
        });
        return th;
    };

    headerRow.appendChild(createHeader('Symbol', 'name'));
    headerRow.appendChild(createHeader('Self', 'self'));
    headerRow.appendChild(createHeader('Total', 'total'));
    thead.appendChild(headerRow);
    table.appendChild(thead);

    const tbody = document.createElement('tbody');
    functions.forEach(fn => {
        const row = document.createElement('tr');
        row.classList.toggle('selected', fn.name === selectedFunctionName);
        row.addEventListener('click', () => {
            selectedFunctionName = fn.name;
            renderSandwichTable(container, data);

            const rightPanel = document.getElementById('sandwich-right-' + container.parentElement.id);
            fetchSandwichDetail(fn.name, activeTab, resourceFilterParams(resourceFilters), rightPanel);
        });

        const nameCell = document.createElement('td');
        nameCell.className = 'function-name';
        nameCell.textContent = fn.name;
        row.appendChild(nameCell);

        const selfCell = document.createElement('td');
        selfCell.className = 'metric';
        selfCell.textContent = formatTime(fn.self);
        row.appendChild(selfCell);

        const totalCell = document.createElement('td');
        totalCell.className = 'metric';
        totalCell.textContent = formatTime(fn.total);
        row.appendChild(totalCell);

        tbody.appendChild(row);
    });

    table.appendChild(tbody);
    container.appendChild(table);
}

function renderDAGGraph(container, sandwichData) {
    if (!sandwichData || !container) return;
    container.innerHTML = '';

    const width = container.clientWidth;
    const height = Math.max(300, container.clientHeight - 40);

    // Build node and link data
    let selectedName = null;
    let callersRoot = null;
    let calleesRoot = null;

    if (sandwichData.callers) {
        selectedName = sandwichData.callers.name;
        callersRoot = sandwichData.callers;
    }
    if (sandwichData.callees) {
        if (!selectedName) selectedName = sandwichData.callees.name;
        calleesRoot = sandwichData.callees;
    }

    if (!selectedName) return;

    const nodes = [];
    const links = [];
    const nodeMap = new Map();

    // Add center node (selected frame)
    const centerNode = { id: 'center', name: selectedName, type: 'center', value: null, x: width / 2, y: height / 2 };
    nodes.push(centerNode);
    nodeMap.set('center', centerNode);

    // Helper to flatten tree and limit depth to 7
    function addNodesFromTree(root, isCallers, maxDepth = 7) {
        if (!root || !root.children) return;

        const nodesByDepth = [];

        function traverse(node, depth, parentId) {
            if (depth > maxDepth || !node) return;

            const key = `${isCallers ? 'C' : 'E'}_${depth}_${parentId}_${node.name}`;
            let currentNode;

            if (!nodeMap.has(key)) {
                currentNode = {
                    id: key,
                    name: node.name,
                    type: isCallers ? 'caller' : 'callee',
                    value: node.value,
                    depth: depth,
                    frametype: node.frametype || '',
                    l: node.l,
                    libtype: node.libtype
                };
                nodes.push(currentNode);
                nodeMap.set(key, currentNode);
                if (!nodesByDepth[depth]) nodesByDepth[depth] = [];
                nodesByDepth[depth].push(currentNode);
            } else {
                currentNode = nodeMap.get(key);
            }

            // Add link
            links.push({ source: isCallers ? currentNode.id : parentId, target: isCallers ? parentId : currentNode.id });

            // Recursively add children (limit to 5 per frame)
            if (node.children) {
                node.children.slice(0, 5).forEach(child => {
                    traverse(child, depth + 1, currentNode.id);
                });
            }
        }

        // Start traversal from root's children (limit to maxDepth immediate callers/callees)
        if (root.children) {
            root.children.slice(0, maxDepth).forEach(child => {
                traverse(child, 1, 'center');
            });
        }
    }

    // Add callers (flowing up to the selected frame)
    if (callersRoot) {
        addNodesFromTree(callersRoot, true);
    }

    // Add callees (flowing down from the selected frame)
    if (calleesRoot) {
        addNodesFromTree(calleesRoot, false);
    }

    if (nodes.length <= 1) {
        container.innerHTML = '<div class="status-box">No callers or callees to display</div>';
        return;
    }

    // Calculate positions using a simple tree layout
    const nodeWidth = 280;
    const charWidth = 7;
    const verticalSpacing = 80;
    const horizontalSpacing = 340;

    // Helper function to wrap text and calculate required height
    function calculateTextLines(text, maxWidth) {
        const maxChars = Math.floor(maxWidth / charWidth);
        if (text.length <= maxChars) return [text];

        const lines = [];
        let currentLine = '';
        const words = text.split(/([._-])/);

        for (const word of words) {
            if ((currentLine + word).length <= maxChars) {
                currentLine += word;
            } else {
                if (currentLine) lines.push(currentLine);
                currentLine = word;
            }
        }
        if (currentLine) lines.push(currentLine);
        return lines;
    }

    // Calculate node heights based on text content
    const lineHeight = 14;
    const nodePaddingV = 8;
    const sidePadding = 5;
    nodes.forEach(n => {
        const lines = calculateTextLines(n.name, nodeWidth - sidePadding * 2);
        n.textLines = lines;
        n.nodeHeight = Math.max(40, lines.length * lineHeight + nodePaddingV * 2);
    });

    const nodeHeight = 40;

    // Group nodes by type and depth
    const callersByDepth = {};
    const calleesByDepth = {};

    nodes.forEach(n => {
        if (n.type === 'center') return;
        const depth = n.depth || 1;
        if (n.type === 'caller') {
            if (!callersByDepth[depth]) callersByDepth[depth] = [];
            callersByDepth[depth].push(n);
        } else {
            if (!calleesByDepth[depth]) calleesByDepth[depth] = [];
            calleesByDepth[depth].push(n);
        }
    });

    // Position callers (above center)
    Object.keys(callersByDepth).sort().reverse().forEach(d => {
        const depth = parseInt(d);
        const nodesAtDepth = callersByDepth[d];
        const numNodes = nodesAtDepth.length;
        const y = centerNode.y - depth * verticalSpacing;
        const totalWidth = numNodes * horizontalSpacing;
        const startX = width / 2 - totalWidth / 2;

        nodesAtDepth.forEach((n, i) => {
            n.x = startX + i * horizontalSpacing;
            n.y = y;
        });
    });

    // Position callees (below center)
    Object.keys(calleesByDepth).sort().forEach(d => {
        const depth = parseInt(d);
        const nodesAtDepth = calleesByDepth[d];
        const numNodes = nodesAtDepth.length;
        const y = centerNode.y + depth * verticalSpacing;
        const totalWidth = numNodes * horizontalSpacing;
        const startX = width / 2 - totalWidth / 2;

        nodesAtDepth.forEach((n, i) => {
            n.x = startX + i * horizontalSpacing;
            n.y = y;
        });
    });

    // Calculate bounds of all nodes and center the DAG
    let minX = Infinity, maxX = -Infinity;
    let minY = Infinity, maxY = -Infinity;
    nodes.forEach(n => {
        minX = Math.min(minX, n.x - nodeWidth / 2);
        maxX = Math.max(maxX, n.x + nodeWidth / 2);
        minY = Math.min(minY, n.y - n.nodeHeight / 2);
        maxY = Math.max(maxY, n.y + n.nodeHeight / 2);
    });

    // Calculate offset to center the DAG
    const padding = 40;
    const graphWidth = maxX - minX + padding * 2;
    const graphHeight = maxY - minY + padding * 2;
    const offsetX = (width - graphWidth) / 2 - minX + padding;
    const offsetY = (height - graphHeight) / 2 - minY + padding;

    // Apply offset to all nodes
    nodes.forEach(n => {
        n.x += offsetX;
        n.y += offsetY;
    });

    // Create SVG
    const svg = d3.select(container).append('svg')
        .attr('width', width)
        .attr('height', height)
        .style('background-color', 'white');

    // Create a group for zoom/pan
    const g = svg.append('g');

    // Define arrow markers
    svg.append('defs').append('marker')
        .attr('id', 'arrowhead')
        .attr('markerWidth', 10)
        .attr('markerHeight', 10)
        .attr('refX', 9)
        .attr('refY', 3)
        .attr('orient', 'auto')
        .append('polygon')
        .attr('points', '0 0, 10 3, 0 6')
        .attr('fill', '#666');

    // Draw links
    g.selectAll('line.link')
        .data(links)
        .join('line')
        .attr('class', 'link')
        .attr('x1', d => {
            const sourceNode = nodeMap.get(d.source);
            return sourceNode.x;
        })
        .attr('y1', d => {
            const sourceNode = nodeMap.get(d.source);
            return sourceNode.y + sourceNode.nodeHeight / 2;
        })
        .attr('x2', d => {
            const targetNode = nodeMap.get(d.target);
            return targetNode.x;
        })
        .attr('y2', d => {
            const targetNode = nodeMap.get(d.target);
            return targetNode.y - targetNode.nodeHeight / 2;
        })
        .attr('stroke', '#999')
        .attr('stroke-width', 2)
        .attr('marker-end', 'url(#arrowhead)');

    // Draw nodes
    const nodes_g = g.selectAll('g.node')
        .data(nodes)
        .join('g')
        .attr('class', 'node')
        .attr('transform', d => `translate(${d.x},${d.y})`);

    // Color mapper for frame types (same as flamegraph)
    const colorMapper = function(d) {
        if (d.type === 'center') return '#2c3e50';

        return frameColor(d.frametype, d.name) || '#cccccc';
    };

    nodes_g.append('rect')
        .attr('width', nodeWidth)
        .attr('height', d => d.nodeHeight)
        .attr('x', -nodeWidth / 2)
        .attr('y', d => -d.nodeHeight / 2)
        .attr('fill', colorMapper)
        .attr('stroke', '#333')
        .attr('stroke-width', 1)
        .attr('rx', 4);

    nodes_g.append('text')
        .attr('text-anchor', 'middle')
        .attr('dominant-baseline', 'middle')
        .attr('font-size', '12px')
        .attr('font-family', 'monospace')
        .attr('fill', d => d.type === 'center' ? 'white' : '#000')
        .attr('pointer-events', 'none')
        .attr('y', d => {
            const numLines = d.textLines ? d.textLines.length : 1;
            return -(numLines - 1) * lineHeight / 2;
        })
        .selectAll('tspan')
        .data(d => d.textLines || [d.name])
        .join('tspan')
        .attr('x', 0)
        .attr('dy', (d, i) => i === 0 ? 0 : lineHeight)
        .text(d => d);

    // Add zoom behavior
    const zoom = d3.zoom()
        .on('zoom', (event) => {
            g.attr('transform', event.transform);
        });

    svg.call(zoom);

    // Add tooltip
    const tooltip = d3.select(container).append('div')
        .style('position', 'absolute')
        .style('background', 'rgba(0, 0, 0, 0.8)')
        .style('color', 'white')
        .style('padding', '8px 12px')
        .style('border-radius', '4px')
        .style('font-size', '11px')
        .style('font-family', 'monospace')
        .style('pointer-events', 'none')
        .style('display', 'none')
        .style('z-index', '1000')
        .style('max-width', '400px')
        .style('word-break', 'break-all');

    nodes_g.on('mouseover', function(event, d) {
        tooltip.style('display', 'block')
            .style('left', (event.pageX + 12) + 'px')
            .style('top', (event.pageY - 28) + 'px')
            .html(null);
        tooltip.append('strong').text(d.name);
        if (d.value) {
            tooltip.append('br');
            tooltip.append('span').text('Samples: ' + Math.round(d.value));
        }
    })
    .on('mouseleave', () => {
        tooltip.style('display', 'none');
    });
}

function setupSandwichDivider(divider, leftPanel) {
    let isResizing = false;
    let startX = 0;
    let startWidth = 0;

    divider.addEventListener('mousedown', (e) => {
        isResizing = true;
        startX = e.clientX;
        startWidth = leftPanel.clientWidth;
        document.body.style.userSelect = 'none';
    });

    document.addEventListener('mousemove', (e) => {
        if (!isResizing) return;
        const delta = e.clientX - startX;
        const newWidth = Math.max(200, startWidth + delta);
        const container = divider.parentElement;
        const maxWidth = Math.max(200, container.clientWidth - 200);
        if (newWidth <= maxWidth) {
            leftPanel.style.flex = `0 0 ${newWidth}px`;
        }
    });

    document.addEventListener('mouseup', () => {
        isResizing = false;
        document.body.style.userSelect = '';
    });
}

function init() {
    // Load the configured default virtual frames before the first render.
    fetch(window.__BASE_PATH__ + '/api/config')
        .then(r => r.json())
        .then(cfg => {
            if (cfg && Array.isArray(cfg.virtualFrames)) {
                virtualFrames = cfg.virtualFrames;
                rebuildVirtualFrameControls();
            }
        })
        .catch(() => {})
        .finally(() => {
            updateChart(true);
            startUpdates();
        });
    document.getElementById('pause-btn').addEventListener('click', togglePause);
    document.getElementById('view-select').addEventListener('change', (e) => {
        onViewModeChange(e.target.value);
    });
    document.getElementById('resource-key-select').addEventListener('change', (e) => {
        selectedAttrKey = e.target.value;
        rebuildValueSelect();
    });
    document.getElementById('resource-value-select').addEventListener('change', (e) => {
        if (!selectedAttrKey || !e.target.value) return;
        resourceFilters = [...resourceFilters, { key: selectedAttrKey, value: e.target.value }];
        selectedAttrKey = '';
        rebuildResourceFilterControls();
        updateChart(true);
    });
    document.getElementById('vframe-add-select').addEventListener('change', (e) => {
        const key = e.target.value;
        if (!key) return;
        virtualFrames = [...virtualFrames, key];
        rebuildVirtualFrameControls();
        updateChart(true);
    });
    document.getElementById('frame-filter').addEventListener('input', applyFrameFilter);
    document.getElementById('export-btn').addEventListener('click', exportCurrentGraph);
    document.getElementById('sort-select').addEventListener('change', (e) => {
        sortMode = e.target.value;
        if (viewMode === 'flamegraph') updateChart(true);
    });
}

if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => setTimeout(init, 500));
} else {
    setTimeout(init, 500);
}
