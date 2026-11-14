// Helpers of the firepit web UI that neither touch the DOM nor d3, so they
// can be unit tested without a browser: see ../web_test.

// frameTypeColor returns the color of a frame of the given
// profile.frame.type, or null for unknown types. saturation, between 0 and
// 1, varies the color slightly.
export function frameTypeColor(frameType, saturation) {
    const c = (b, r) => b + Math.round(r * saturation);
    switch (frameType) {
        case 'kernel': return `rgb(${c(163,75)},${c(195,49)},${c(72,149)})`;
        case 'native': return `rgb(${c(200,55)},${c(50,80)},${c(50,80)})`;
        case 'cpython': return `rgb(${c(91,126)},${c(156,76)},${c(221,26)})`;
        case 'jvm':    return `rgb(${c(50,60)},${c(200,55)},${c(50,60)})`;
        case 'dotnet': return `rgb(${c(150,55)},${c(50,80)},${c(200,40)})`;
        case 'v8js':   return `rgb(${c(175,55)},${c(175,55)},${c(50,20)})`;
        case 'ruby':   return `rgb(${c(190,65)},${c(90,65)},0)`;
        case 'perl':   return `rgb(${c(50,60)},${c(165,55)},${c(165,55)})`;
        case 'php':    return `rgb(${c(119,40)},${c(123,40)},${c(179,40)})`;
        case 'beam':   return `rgb(${c(220,35)},${c(120,50)},${c(80,40)})`;
        case 'go':     return `rgb(${c(0,50)},${c(173,45)},${c(196,40)})`;
        case 'rust':   return `rgb(${c(183,50)},${c(65,40)},${c(14,25)})`;
        case 'luajit': return `rgb(${c(0,50)},${c(160,50)},${c(210,40)})`;
        case 'virtual': return `rgb(${c(150,40)},${c(155,40)},${c(165,40)})`;
        default:       return null;
    }
}

// nameSaturation derives a value between 0 and 1 from the start of a
// function name. It reproduces the name hash of d3-flame-graph, so frames of
// the same function get the same shade.
export function nameSaturation(name) {
    const sym = (name || '').split('(')[0];
    let sum = 0, weight = 0, w = 1;
    for (let i = 0; i < sym.length && i <= 6; i++) {
        sum += w * (sym.charCodeAt(i) % 10);
        weight += 9 * w;
        w *= 0.7;
    }
    return weight > 0 ? sum / weight : 0;
}

// frameColor returns the color of a frame, or null if its frame type is
// unset or unknown.
export function frameColor(frameType, name) {
    if (!frameType) return null;
    return frameTypeColor(frameType, nameSaturation(name));
}

// frameLabel returns the description of a frame. fraction is the share of
// the frame in all samples, between 0 and 1.
export function frameLabel(name, fraction, samples, filename) {
    const base = name + ' (' + (100 * fraction).toFixed(3) + '%, ' + samples + ' samples)';
    return filename ? base + ' — ' + filename : base;
}

// toQuery returns the query string of params, including the leading
// question mark, or the empty string if there are no parameters.
export function toQuery(params) {
    const qs = params.toString();
    return qs ? '?' + qs : '';
}

// sanitizeId returns str with all characters not valid in an element id replaced.
export function sanitizeId(str) {
    return String(str).replace(/[^A-Za-z0-9_-]/g, '_');
}

// hasData reports whether a flame graph has any frames.
export function hasData(root) {
    return Boolean(root && root.children && root.children.length > 0);
}

// hasHeatMapData reports whether a heatmap covers at least one second.
export function hasHeatMapData(d) {
    return Boolean(d && d.numSeconds > 0);
}

// hasSandwichData reports whether a sandwich view has any functions.
export function hasSandwichData(d) {
    return Boolean(d && d.functions && d.functions.length > 0);
}

// heatMapCells returns the non-zero cells of a heatmap as {xi, yi, v}, with
// xi the second and yi the sub-bucket.
export function heatMapCells(d) {
    const cells = [];
    for (let xi = 0; xi < d.numSeconds; xi++) {
        for (let yi = 0; yi < d.numSubBuckets; yi++) {
            const v = d.cells[xi * d.numSubBuckets + yi];
            if (v > 0) cells.push({ xi, yi, v });
        }
    }
    return cells;
}

// wrapText splits text into lines of at most maxChars characters, breaking
// after or before '.', '_' and '-'. Longer pieces get a line of their own.
export function wrapText(text, maxChars) {
    if (text.length <= maxChars) return [text];

    const lines = [];
    let currentLine = '';
    for (const word of text.split(/([._-])/)) {
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

export function formatTime(value) {
    return Math.round(value).toString();
}

// sortSandwichFunctions sorts functions in place by column: 'name', 'self'
// or anything else for the total.
export function sortSandwichFunctions(functions, column, ascending) {
    functions.sort((a, b) => {
        let order;
        if (column === 'name') {
            order = a.name.localeCompare(b.name);
        } else if (column === 'self') {
            order = a.self - b.self;
        } else {
            order = a.total - b.total;
        }
        return ascending ? order : -order;
    });
    return functions;
}

// resourceFilterParams returns the resourceType query values of filters,
// a list of {key, value}.
export function resourceFilterParams(filters) {
    return filters.map(f => f.key + ':' + f.value);
}

// offeredResourceFilters returns the filters whose value attributes, a map
// of key to values, still offers. Other filters would hide all profiles.
export function offeredResourceFilters(filters, attributes) {
    return filters.filter(f => (attributes.get(f.key) || []).includes(f.value));
}

// matchesFrameFilter reports whether a frame is highlighted by the frame
// filter. filterText is expected in lower case.
export function matchesFrameFilter(name, filterText) {
    const frameText = (name || '').toString().toLowerCase();
    return frameText !== '' && frameText.includes(filterText);
}

// buildExport returns the document that is saved when a flame graph is
// exported.
export function buildExport(graph, resourceType, virtualFrames, now) {
    return {
        type: graph.type,
        unit: graph.unit || null,
        data: graph.root,
        exportedAt: now.toISOString(),
        resourceType: resourceType || 'all',
        virtualFrames: virtualFrames
    };
}

export function exportFilename(type, now) {
    return `flamegraph-${type}-${now.getTime()}.json`;
}
