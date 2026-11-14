// Unit tests of the helpers of the web UI. Run with: make test-web

import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
    buildExport,
    exportFilename,
    formatTime,
    frameColor,
    frameLabel,
    frameTypeColor,
    hasData,
    hasHeatMapData,
    hasSandwichData,
    heatMapCells,
    matchesFrameFilter,
    nameSaturation,
    offeredResourceFilters,
    resourceFilterParams,
    sanitizeId,
    sortSandwichFunctions,
    toQuery,
    wrapText,
} from '../web/lib.js';

test('frameTypeColor maps frame types to colors within the RGB range', () => {
    assert.equal(frameTypeColor('go', 0), 'rgb(0,173,196)');
    assert.equal(frameTypeColor('ruby', 1), 'rgb(255,155,0)');
    assert.equal(frameTypeColor('unknown', 0.5), null);
    const types = ['kernel', 'native', 'cpython', 'jvm', 'dotnet', 'v8js', 'ruby', 'perl', 'php', 'beam', 'go', 'rust', 'luajit', 'virtual'];
    for (const type of types) {
        for (const saturation of [0, 0.25, 0.5, 1]) {
            const color = frameTypeColor(type, saturation);
            const m = /^rgb\((\d+),(\d+),(\d+)\)$/.exec(color);
            assert.ok(m, `${type}: ${color}`);
            for (const channel of m.slice(1)) {
                assert.ok(Number(channel) <= 255, `${type} at ${saturation}: ${color}`);
            }
        }
    }
});

test('nameSaturation is between 0 and 1', () => {
    assert.equal(nameSaturation(undefined), 0);
    assert.equal(nameSaturation('(anonymous)'), 0);
    for (const name of ['main', 'java.lang.Thread.run()', 'z'.repeat(40), '☃']) {
        const s = nameSaturation(name);
        assert.ok(s >= 0 && s <= 1, `${name}: ${s}`);
    }
    // Only the symbol before the arguments and its first characters count.
    assert.equal(nameSaturation('foo(int)'), nameSaturation('foo(char*)'));
    assert.equal(nameSaturation('abcdefgXXX'), nameSaturation('abcdefgYYY'));
    assert.notEqual(nameSaturation('aaaa'), nameSaturation('bbbb'));
});

test('frameColor needs a known frame type', () => {
    assert.equal(frameColor('', 'main'), null);
    assert.equal(frameColor(undefined, 'main'), null);
    assert.equal(frameColor('cobol', 'main'), null);
    assert.equal(frameColor('go', 'main'), 'rgb(36,205,225)');
    assert.match(frameColor('native', undefined), /^rgb\(/);
});

test('frameLabel describes a frame', () => {
    assert.equal(frameLabel('main', 0.5, 42), 'main (50.000%, 42 samples)');
    assert.equal(frameLabel('main', 1, 7, 'main.go:12'), 'main (100.000%, 7 samples) — main.go:12');
    assert.equal(frameLabel('f', 1 / 3, 1, ''), 'f (33.333%, 1 samples)');
    assert.equal(frameLabel('', 0, 0), ' (0.000%, 0 samples)');
});

test('toQuery prefixes non-empty queries', () => {
    assert.equal(toQuery(new URLSearchParams()), '');
    assert.equal(toQuery(new URLSearchParams({ a: '1' })), '?a=1');
    assert.equal(toQuery(new URLSearchParams({ resourceType: 'service.name:a b', virtualFrames: '' })),
        '?resourceType=service.name%3Aa+b&virtualFrames=');
});

test('sanitizeId replaces characters not valid in an id', () => {
    assert.equal(sanitizeId('samples(count)'), 'samples_count_');
    assert.equal(sanitizeId('cpu'), 'cpu');
    assert.equal(sanitizeId('alloc "space" <bytes>'), 'alloc__space___bytes_');
    assert.equal(sanitizeId(42), '42');
});

test('hasData requires children', () => {
    assert.equal(hasData(null), false);
    assert.equal(hasData(undefined), false);
    assert.equal(hasData({ name: 'root', value: 0 }), false);
    assert.equal(hasData({ name: 'root', children: [] }), false);
    assert.equal(hasData({ name: 'root', children: [{ name: 'main', value: 1 }] }), true);
});

test('hasHeatMapData requires seconds', () => {
    assert.equal(hasHeatMapData(null), false);
    assert.equal(hasHeatMapData({ numSeconds: 0 }), false);
    assert.equal(hasHeatMapData({ numSeconds: 3 }), true);
});

test('hasSandwichData requires functions', () => {
    assert.equal(hasSandwichData(null), false);
    assert.equal(hasSandwichData({ root: null }), false);
    assert.equal(hasSandwichData({ functions: [] }), false);
    assert.equal(hasSandwichData({ functions: [{ name: 'main', self: 1, total: 1 }] }), true);
});

test('heatMapCells flattens the non-zero cells', () => {
    assert.deepEqual(heatMapCells({ numSeconds: 0, numSubBuckets: 2, cells: [] }), []);
    assert.deepEqual(heatMapCells({ numSeconds: 2, numSubBuckets: 3, cells: [0, 4, 0, 1, 0, 2] }), [
        { xi: 0, yi: 1, v: 4 },
        { xi: 1, yi: 0, v: 1 },
        { xi: 1, yi: 2, v: 2 },
    ]);
});

test('wrapText breaks at separators', () => {
    assert.deepEqual(wrapText('main', 10), ['main']);
    assert.deepEqual(wrapText('', 10), ['']);
    assert.deepEqual(wrapText('runtime.mallocgc', 12), ['runtime.', 'mallocgc']);
    assert.deepEqual(wrapText('a_b-c.d', 3), ['a_b', '-c.', 'd']);
    assert.deepEqual(wrapText('averylongword.x', 5), ['averylongword', '.x']);
});

test('offeredResourceFilters drops filters no longer offered', () => {
    const attrs = new Map([['service.name', ['api', 'db']], ['host', []]]);
    const filters = [
        { key: 'service.name', value: 'api' },
        { key: 'service.name', value: 'gone' },
        { key: 'host', value: 'a' },
        { key: 'missing', value: 'x' },
    ];
    assert.deepEqual(offeredResourceFilters(filters, attrs), [{ key: 'service.name', value: 'api' }]);
});

test('formatTime rounds to integers', () => {
    assert.equal(formatTime(12), '12');
    assert.equal(formatTime(12.5), '13');
    assert.equal(formatTime(0.2), '0');
});

test('sortSandwichFunctions sorts by column and direction', () => {
    const functions = () => [
        { name: 'b', self: 1, total: 30 },
        { name: 'c', self: 5, total: 10 },
        { name: 'a', self: 3, total: 20 },
    ];
    const names = (column, ascending) => sortSandwichFunctions(functions(), column, ascending).map(f => f.name).join('');
    assert.equal(names('total', false), 'bac');
    assert.equal(names('total', true), 'cab');
    assert.equal(names('self', false), 'cab');
    assert.equal(names('self', true), 'bac');
    assert.equal(names('name', true), 'abc');
    assert.equal(names('name', false), 'cba');

    // Sorts in place.
    const list = functions();
    assert.equal(sortSandwichFunctions(list, 'name', true), list);
    assert.equal(list[0].name, 'a');
});

test('resourceFilterParams joins keys and values', () => {
    assert.deepEqual(resourceFilterParams([]), []);
    assert.deepEqual(
        resourceFilterParams([{ key: 'service.name', value: 'api' }, { key: 'url', value: 'http://x' }]),
        ['service.name:api', 'url:http://x']);
});

test('matchesFrameFilter compares case-insensitively', () => {
    assert.equal(matchesFrameFilter('runtime.mallocGC', 'mallocgc'), true);
    assert.equal(matchesFrameFilter('runtime.mallocGC', 'free'), false);
    assert.equal(matchesFrameFilter('', 'x'), false);
    assert.equal(matchesFrameFilter(undefined, 'x'), false);
    assert.equal(matchesFrameFilter(12345, '234'), true);
});

test('buildExport describes the exported graph', () => {
    const now = new Date(Date.UTC(2026, 0, 2, 3, 4, 5));
    const root = { name: 'root', value: 1 };
    assert.deepEqual(buildExport({ type: 'cpu', unit: 'ns', root }, 'service.name:api', ['thread.name'], now), {
        type: 'cpu',
        unit: 'ns',
        data: root,
        exportedAt: '2026-01-02T03:04:05.000Z',
        resourceType: 'service.name:api',
        virtualFrames: ['thread.name'],
    });
    const plain = buildExport({ type: 'cpu', root }, '', [], now);
    assert.equal(plain.unit, null);
    assert.equal(plain.resourceType, 'all');
});

test('exportFilename names the type and time', () => {
    const now = new Date(Date.UTC(2026, 0, 2, 3, 4, 5));
    assert.equal(exportFilename('samples(count)', now), 'flamegraph-samples(count)-1767323045000.json');
});
