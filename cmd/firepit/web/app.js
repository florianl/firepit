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
    hasHeatMapData,
    hasSandwichData,
    heatMapCells,
    matchesFrameFilter,
    offeredResourceFilters,
    resourceFilterParams,
    sanitizeId,
    sortSandwichFunctions,
    toQuery,
    wrapText,
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
    startPolling();
    if (typeof EventSource === 'undefined') return;
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
let frameFilter = '';
let currentGraphs = null;
let viewMode = 'flamegraph';
let sortMode = 'time-ordered';
// Attribute keys rendered as virtual frames in the flame graph view,
// outermost first. None are selected unless configured on the server.
let virtualFrames = [];
let attributeKeys = [];

// chipElement returns a chip showing text with a button that calls onRemove.
function chipElement(text, title, onRemove) {
    const chip = document.createElement('span');
    chip.className = 'vframe-chip';
    chip.textContent = text;
    const remove = document.createElement('button');
    remove.type = 'button';
    remove.title = title;
    remove.textContent = '×';
    remove.addEventListener('click', onRemove);
    chip.appendChild(remove);
    return chip;
}

// fillSelect replaces the options of select with a placeholder, which has
// the empty value, and one option per value.
function fillSelect(select, placeholder, values) {
    select.replaceChildren(new Option(placeholder, ''), ...values.map(v => new Option(v, v)));
}

// Rebuilds the filter chips and the selects. Filtered keys are not
// offered again, as a resource has one value per key.
function rebuildResourceFilterControls() {
    document.getElementById('resource-filter-chips').replaceChildren(
        ...resourceFilters.map((f, i) => chipElement(f.key + ' = ' + f.value, 'Remove filter ' + f.key, () => {
            resourceFilters = resourceFilters.filter((_, j) => j !== i);
            rebuildResourceFilterControls();
            updateChart(true);
        })));

    const used = new Set(resourceFilters.map(f => f.key));
    const keys = [...resourceAttributeMap.keys()].filter(k => !used.has(k));
    if (!keys.includes(selectedAttrKey)) selectedAttrKey = '';

    const keySelect = document.getElementById('resource-key-select');
    fillSelect(keySelect, resourceFilters.length ? '+ and' : 'All', keys);
    keySelect.value = selectedAttrKey;
    keySelect.disabled = keys.length === 0;

    rebuildValueSelect();
}

function rebuildValueSelect() {
    const valueSelect = document.getElementById('resource-value-select');
    if (!selectedAttrKey) {
        fillSelect(valueSelect, '(select attribute)', []);
        valueSelect.disabled = true;
        return;
    }
    fillSelect(valueSelect, '(select value)', resourceAttributeMap.get(selectedAttrKey) || []);
    valueSelect.disabled = false;
    valueSelect.value = '';
}

function rebuildVirtualFrameControls() {
    document.getElementById('vframe-chips').replaceChildren(
        ...virtualFrames.map((key, i) => chipElement(key, 'Remove ' + key, () => {
            virtualFrames = virtualFrames.filter((_, j) => j !== i);
            rebuildVirtualFrameControls();
            updateChart(true);
        })));

    const addSelect = document.getElementById('vframe-add-select');
    const keys = attributeKeys.filter(k => !virtualFrames.includes(k));
    fillSelect(addSelect, '+ add', keys);
    addSelect.value = '';
    addSelect.disabled = keys.length === 0;
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

    // Classes go on the group of each frame rectangle.
    document.querySelectorAll('svg rect').forEach(rect => {
        const parent = rect.parentElement;
        if (!parent || parent.tagName !== 'g') return;
        const frameName = rect.__data__ && rect.__data__.data ? rect.__data__.data.name : '';
        const match = filterText !== '' && matchesFrameFilter(frameName, filterText);
        parent.classList.toggle('flame-dimmed', filterText !== '' && !match);
        parent.classList.toggle('flame-highlight', match);
    });
}

// Frames narrower than this many pixels are not drawn. Zooming in shows them.
const MIN_FRAME_PX = 0.7;

function renderFlamegraph(containerId, data) {
    const container = document.getElementById(containerId);
    if (!container) return;
    container.replaceChildren();

    const width = document.documentElement.clientWidth;
    const height = Math.max(400, window.innerHeight - 220);

    const sortFn = sortMode === 'left-heavy'
        ? (a, b) => b.value - a.value
        : false;

    const fg = flamegraph.default()
        .width(width)
        .height(height)
        .sort(sortFn)
        .minFrameSize(MIN_FRAME_PX)
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

    if (frameFilter) {
        setTimeout(applyFrameFilter, 0);
    }

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
// Aborts the pending view request when a newer one starts.
let viewAbort = null;

// httpError returns the error of a failed response r.
function httpError(r) {
    return new Error('HTTP ' + r.status + (r.statusText ? ' ' + r.statusText : ''));
}

// isAbort reports whether err comes from a request superseded by a newer one.
function isAbort(err) {
    return err && err.name === 'AbortError';
}

// fetchViewData resolves to {data}, or to null if the view is already
// rendered from this response and force is not set. It rejects with an
// AbortError once a newer request starts.
function fetchViewData(endpoint, query, force) {
    const url = window.__BASE_PATH__ + endpoint + query;
    if (viewAbort !== null) viewAbort.abort();
    viewAbort = new AbortController();
    return fetch(url, { signal: viewAbort.signal }).then(r => {
        if (!r.ok) throw httpError(r);
        const etag = r.headers.get('ETag');
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
        if (!isAbort(err)) lastRenderedKey = null;
        throw err;
    });
}

// showTooltip shows the tooltip with the given nodes or strings next to
// the pointer of event.
function showTooltip(event, ...content) {
    const tooltip = document.getElementById('tooltip');
    tooltip.replaceChildren(...content);
    tooltip.style.display = 'block';
    tooltip.style.left = (event.clientX + 12) + 'px';
    tooltip.style.top = (event.clientY - 28) + 'px';
}

function hideTooltip() {
    document.getElementById('tooltip').style.display = 'none';
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
    fetchIfChanged('/api/attribute-keys', keys => {
        attributeKeys = keys;
        rebuildVirtualFrameControls();
    });

    // Resource attributes for filtering
    fetchIfChanged('/api/resource-attributes', attrs => {
        resourceAttributeMap = new Map(attrs.map(a => [a.key, a.values || []]));

        // Drop stale filters as they would hide all profiles
        const before = resourceFilters.length;
        resourceFilters = offeredResourceFilters(resourceFilters, resourceAttributeMap);
        rebuildResourceFilterControls();
        if (resourceFilters.length !== before) updateChart(true);
    });
}

// Last response of each endpoint polled by fetchIfChanged.
const lastListKeys = new Map();

// fetchIfChanged fetches a JSON list from endpoint and passes it to
// onChange if it differs from the previous response.
function fetchIfChanged(endpoint, onChange) {
    fetch(window.__BASE_PATH__ + endpoint)
        .then(r => r.json())
        .then(list => {
            const value = list || [];
            const key = JSON.stringify(value);
            if (key === lastListKeys.get(endpoint)) return;
            lastListKeys.set(endpoint, key);
            onChange(value);
        })
        .catch(() => {});
}

// Renders the panels of the current view by sample type. A panel is
// rendered once its tab is shown, as hidden ones cost as much as visible ones.
let pendingPanels = new Map();

// renderActivePanel renders the panel of the active tab if it is pending.
function renderActivePanel() {
    const render = pendingPanels.get(activeTab);
    if (!render) return;
    pendingPanels.delete(activeTab);
    render();
}

// selectTab shows the tab and panel of the sample type.
function selectTab(type) {
    activeTab = type;
    document.querySelectorAll('.tab-btn').forEach(
        b => b.classList.toggle('active', b.dataset.type === activeTab)
    );
    document.querySelectorAll('.tab-panel').forEach(
        p => p.classList.toggle('active', p.id === 'panel-' + sanitizeId(activeTab))
    );
    renderActivePanel();
}

// renderTabbedView fetches a view with one tab per sample type and renders
// each entry with data into its panel with renderPanel(panelId, entry).
// onData, if set, receives the fetched entries. onSelect, if set, runs when
// the user switches tabs.
function renderTabbedView(endpoint, query, force, { hasEntryData, renderPanel, panelClass = '', onData, onSelect }) {
    fetchViewData(endpoint, query, force)
        .then(res => {
            if (res === null) { markUpdated(); return; }
            const entries = res.data;
            if (onData) onData(entries);
            const strip  = document.getElementById('tab-strip');
            const charts = document.getElementById('charts');

            if (!entries || entries.length === 0 || !entries.some(hasEntryData)) {
                strip.style.display = 'none';
                charts.replaceChildren(messageElement('waiting', 'Waiting for profiles...'));
                markUpdated();
                return;
            }

            if (activeTab === null || !entries.find(e => e.type === activeTab)) {
                activeTab = entries[0].type;
            }

            strip.style.display = 'flex';
            strip.replaceChildren();
            entries.forEach(e => {
                const btn = document.createElement('button');
                btn.className = 'tab-btn' + (e.type === activeTab ? ' active' : '');
                btn.textContent = e.unit ? `${e.type} (${e.unit})` : e.type;
                btn.dataset.type = e.type;
                btn.addEventListener('click', () => {
                    if (onSelect) onSelect();
                    selectTab(e.type);
                });
                strip.appendChild(btn);
            });

            charts.replaceChildren();
            pendingPanels = new Map();
            entries.forEach(e => {
                const panel = document.createElement('div');
                const panelId = 'panel-' + sanitizeId(e.type);
                panel.className = 'tab-panel' + panelClass + (e.type === activeTab ? ' active' : '');
                panel.id = panelId;
                charts.appendChild(panel);

                if (hasEntryData(e)) {
                    pendingPanels.set(e.type, () => renderPanel(panelId, e));
                } else {
                    panel.replaceChildren(messageElement('waiting', 'No data for ' + e.type));
                }
            });
            setTimeout(renderActivePanel, 0);

            markUpdated();
        })
        .catch(err => {
            if (isAbort(err)) return;
            document.getElementById('charts').replaceChildren(errorElement(err));
        });
}

function fetchAndRenderFlamegraph(query, force) {
    renderTabbedView('/api/flamegraph', query, force, {
        hasEntryData: g => hasData(g.root),
        renderPanel: (panelId, g) => renderFlamegraph(panelId, g.root),
        onData: graphs => { currentGraphs = graphs; },
    });
}

function fetchAndRenderHeatMap(query, force) {
    renderTabbedView('/api/flamescope', query, force, {
        hasEntryData: h => hasHeatMapData(h.data),
        renderPanel: (panelId, h) => {
            const panel = document.getElementById(panelId);
            if (panel) renderHeatMap(panel, h.data);
        },
    });
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

    container.replaceChildren();
    const svg = d3.select(container).append('svg')
        .attr('width',  totalWidth)
        .attr('height', totalHeight);

    const g = svg.append('g')
        .attr('transform', `translate(${marginLeft},${marginTop})`);

    const cellData = heatMapCells(d);

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
            showTooltip(event, `t=${wallTime} +${offsetMs}ms  samples=${c.v}`);
        })
        .on('mouseleave', hideTooltip);

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
    // One heatmap per panel, so the gradient id must be unique.
    const gradId = 'hm-legend-grad-' + container.id;
    const grad  = defs.append('linearGradient').attr('id', gradId)
        .attr('x1', '0%').attr('y1', '0%')
        .attr('x2', '100%').attr('y2', '0%');
    grad.append('stop').attr('offset', '0%').attr('stop-color', '#ffffff');
    grad.append('stop').attr('offset', '100%').attr('stop-color', '#8b0000');
    g.append('rect')
        .attr('x', legendX).attr('y', legendY)
        .attr('width', legendW).attr('height', legendH)
        .attr('fill', `url(#${gradId})`);
    g.append('text').attr('x', legendX).attr('y', legendY - 2)
        .attr('font-size', '10px').attr('fill', '#555').text('0');
    g.append('text').attr('x', legendX + legendW).attr('y', legendY - 2)
        .attr('text-anchor', 'end')
        .attr('font-size', '10px').attr('fill', '#555').text(d.maxValue);
}

let sandwichSortColumn = 'total';
let sandwichSortAsc = false;
let selectedFunctionName = null;
// Aborts the pending sandwich detail request when a newer one starts.
let detailAbort = null;

function fetchAndRenderSandwich(query, force) {
    renderTabbedView('/api/sandwich', query, force, {
        hasEntryData: s => hasSandwichData(s.data),
        renderPanel: (panelId, s) => renderSandwichView(panelId, s.type, s.data),
        panelClass: ' sandwich-container',
        onSelect: () => { selectedFunctionName = null; },
    });
}

// renderSandwichView renders the sandwich view of the sample type. The
// selected function stays selected while the active type still has it.
function renderSandwichView(containerId, type, data) {
    const selected = type === activeTab &&
        data.functions.some(fn => fn.name === selectedFunctionName);
    if (type === activeTab && !selected) selectedFunctionName = null;
    const container = document.getElementById(containerId);
    if (!container) return;

    container.replaceChildren();

    const leftPanel = document.createElement('div');
    leftPanel.className = 'sandwich-left';

    const divider = document.createElement('div');
    divider.className = 'sandwich-divider';

    const rightPanel = document.createElement('div');
    rightPanel.className = 'sandwich-right';

    container.appendChild(leftPanel);
    container.appendChild(divider);
    container.appendChild(rightPanel);

    renderSandwichTable(leftPanel, rightPanel, type, data);
    if (selected) {
        fetchSandwichDetail(selectedFunctionName, type, rightPanel);
    } else {
        rightPanel.replaceChildren(messageElement('waiting', 'Please select a function.'));
    }

    setupSandwichDivider(divider, leftPanel);
}

function fetchSandwichDetail(functionName, sampleType, rightPanel) {
    const query = toQuery(appendCommonParams(new URLSearchParams({ functionName, sampleType })));

    if (detailAbort !== null) detailAbort.abort();
    detailAbort = new AbortController();
    fetch(window.__BASE_PATH__ + '/api/sandwich-detail' + query, { signal: detailAbort.signal })
        .then(r => {
            if (!r.ok) throw httpError(r);
            return r.json();
        })
        .then(data => {
            if (data && data[sampleType] && data[sampleType].callers && data[sampleType].callees) {
                const dagSection = document.createElement('div');
                dagSection.className = 'sandwich-graph';
                const dagContainer = document.createElement('div');
                dagContainer.style.height = '100%';
                dagSection.appendChild(dagContainer);
                rightPanel.replaceChildren(dagSection);
                renderDAGGraph(dagContainer, data[sampleType]);
            } else {
                rightPanel.replaceChildren(messageElement('waiting', 'No data for this function.'));
            }
        })
        .catch(err => {
            if (isAbort(err)) return;
            rightPanel.replaceChildren(errorElement(err));
        });
}

// renderSandwichTable renders the function table into container and the
// detail of a clicked function into rightPanel.
function renderSandwichTable(container, rightPanel, type, data) {
    if (!data || !data.functions) return;

    container.replaceChildren();

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
            renderSandwichTable(container, rightPanel, type, data);
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
        row.classList.toggle('selected', type === activeTab && fn.name === selectedFunctionName);
        row.addEventListener('click', () => {
            selectedFunctionName = fn.name;
            renderSandwichTable(container, rightPanel, type, data);
            fetchSandwichDetail(fn.name, activeTab, rightPanel);
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

// Levels of callers and callees shown in the DAG.
const DAG_MAX_DEPTH = 7;
// Direct callers and callees shown in the DAG.
const DAG_MAX_ROOT_CHILDREN = 7;

function renderDAGGraph(container, sandwichData) {
    if (!sandwichData || !container) return;
    container.replaceChildren();

    const width = container.clientWidth;
    const height = Math.max(300, container.clientHeight - 40);

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

    const centerNode = { id: 'center', name: selectedName, type: 'center', value: null, x: width / 2, y: height / 2 };
    nodes.push(centerNode);
    nodeMap.set('center', centerNode);

    // addNodesFromTree adds the callers or callees of root as nodes and links.
    function addNodesFromTree(root, isCallers) {
        if (!root || !root.children) return;

        function traverse(node, depth, parentId) {
            if (depth > DAG_MAX_DEPTH || !node) return;

            const key = `${isCallers ? 'C' : 'E'}_${depth}_${parentId}_${node.name}`;
            let currentNode;

            if (!nodeMap.has(key)) {
                currentNode = {
                    id: key,
                    name: node.name,
                    type: isCallers ? 'caller' : 'callee',
                    value: node.value,
                    depth: depth,
                    frametype: node.frametype || ''
                };
                nodes.push(currentNode);
                nodeMap.set(key, currentNode);
            } else {
                currentNode = nodeMap.get(key);
            }

            links.push({ source: isCallers ? currentNode.id : parentId, target: isCallers ? parentId : currentNode.id });

            // At most 5 children per frame.
            if (node.children) {
                node.children.slice(0, 5).forEach(child => {
                    traverse(child, depth + 1, currentNode.id);
                });
            }
        }

        if (root.children) {
            root.children.slice(0, DAG_MAX_ROOT_CHILDREN).forEach(child => {
                traverse(child, 1, 'center');
            });
        }
    }

    if (callersRoot) {
        addNodesFromTree(callersRoot, true);
    }

    if (calleesRoot) {
        addNodesFromTree(calleesRoot, false);
    }

    if (nodes.length <= 1) {
        container.replaceChildren(messageElement('status-box', 'No callers or callees to display'));
        return;
    }

    const nodeWidth = 280;
    const charWidth = 7;
    const verticalSpacing = 80;
    const horizontalSpacing = 340;

    const lineHeight = 14;
    const nodePaddingV = 8;
    const sidePadding = 5;
    nodes.forEach(n => {
        const lines = wrapText(n.name, Math.floor((nodeWidth - sidePadding * 2) / charWidth));
        n.textLines = lines;
        n.nodeHeight = Math.max(40, lines.length * lineHeight + nodePaddingV * 2);
    });

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

    let minX = Infinity, maxX = -Infinity;
    let minY = Infinity, maxY = -Infinity;
    nodes.forEach(n => {
        minX = Math.min(minX, n.x - nodeWidth / 2);
        maxX = Math.max(maxX, n.x + nodeWidth / 2);
        minY = Math.min(minY, n.y - n.nodeHeight / 2);
        maxY = Math.max(maxY, n.y + n.nodeHeight / 2);
    });

    const padding = 40;
    const graphWidth = maxX - minX + padding * 2;
    const graphHeight = maxY - minY + padding * 2;
    const offsetX = (width - graphWidth) / 2 - minX + padding;
    const offsetY = (height - graphHeight) / 2 - minY + padding;

    nodes.forEach(n => {
        n.x += offsetX;
        n.y += offsetY;
    });

    const svg = d3.select(container).append('svg')
        .attr('width', width)
        .attr('height', height)
        .style('background-color', 'white');

    const g = svg.append('g');

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

    const nodes_g = g.selectAll('g.node')
        .data(nodes)
        .join('g')
        .attr('class', 'node')
        .attr('transform', d => `translate(${d.x},${d.y})`);

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

    const zoom = d3.zoom()
        .on('zoom', (event) => {
            g.attr('transform', event.transform);
        });

    svg.call(zoom);

    nodes_g.on('mouseover', function(event, d) {
        const name = document.createElement('strong');
        name.textContent = d.name;
        if (d.value) {
            showTooltip(event, name, document.createElement('br'), 'Samples: ' + Math.round(d.value));
        } else {
            showTooltip(event, name);
        }
    })
    .on('mouseleave', hideTooltip);
}

function setupSandwichDivider(divider, leftPanel) {
    divider.addEventListener('mousedown', (e) => {
        const startX = e.clientX;
        const startWidth = leftPanel.clientWidth;
        document.body.style.userSelect = 'none';

        const onMove = (e) => {
            const delta = e.clientX - startX;
            const newWidth = Math.max(200, startWidth + delta);
            const container = divider.parentElement;
            const maxWidth = Math.max(200, container.clientWidth - 200);
            if (newWidth <= maxWidth) {
                leftPanel.style.flex = `0 0 ${newWidth}px`;
            }
        };
        const onUp = () => {
            document.removeEventListener('mousemove', onMove);
            document.removeEventListener('mouseup', onUp);
            document.body.style.userSelect = '';
        };
        document.addEventListener('mousemove', onMove);
        document.addEventListener('mouseup', onUp);
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
