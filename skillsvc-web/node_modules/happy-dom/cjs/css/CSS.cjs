"use strict";
var __importDefault = (this && this.__importDefault) || function (mod) {
    return (mod && mod.__esModule) ? mod : { "default": mod };
};
Object.defineProperty(exports, "__esModule", { value: true });
const CSSEscaper_js_1 = __importDefault(require("./utilities/CSSEscaper.cjs"));
const CSSUnitValue_js_1 = __importDefault(require("./CSSUnitValue.cjs"));
/**
 * The CSS interface holds useful CSS-related methods.
 *
 * Reference:
 * https://developer.mozilla.org/en-US/docs/Web/API/CSS.
 */
class CSS {
    Hz = (value) => new CSSUnitValue_js_1.default(value, 'Hz');
    Q = (value) => new CSSUnitValue_js_1.default(value, 'Q');
    ch = (value) => new CSSUnitValue_js_1.default(value, 'ch');
    cm = (value) => new CSSUnitValue_js_1.default(value, 'cm');
    deg = (value) => new CSSUnitValue_js_1.default(value, 'deg');
    dpcm = (value) => new CSSUnitValue_js_1.default(value, 'dpcm');
    dpi = (value) => new CSSUnitValue_js_1.default(value, 'dpi');
    dppx = (value) => new CSSUnitValue_js_1.default(value, 'dppx');
    em = (value) => new CSSUnitValue_js_1.default(value, 'em');
    ex = (value) => new CSSUnitValue_js_1.default(value, 'ex');
    fr = (value) => new CSSUnitValue_js_1.default(value, 'fr');
    grad = (value) => new CSSUnitValue_js_1.default(value, 'grad');
    in = (value) => new CSSUnitValue_js_1.default(value, 'in');
    kHz = (value) => new CSSUnitValue_js_1.default(value, 'kHz');
    mm = (value) => new CSSUnitValue_js_1.default(value, 'mm');
    ms = (value) => new CSSUnitValue_js_1.default(value, 'ms');
    number = (value) => new CSSUnitValue_js_1.default(value, 'number');
    pc = (value) => new CSSUnitValue_js_1.default(value, 'pc');
    percent = (value) => new CSSUnitValue_js_1.default(value, 'percent');
    pt = (value) => new CSSUnitValue_js_1.default(value, 'pt');
    px = (value) => new CSSUnitValue_js_1.default(value, 'px');
    rad = (value) => new CSSUnitValue_js_1.default(value, 'rad');
    rem = (value) => new CSSUnitValue_js_1.default(value, 'rem');
    s = (value) => new CSSUnitValue_js_1.default(value, 's');
    turn = (value) => new CSSUnitValue_js_1.default(value, 'turn');
    vh = (value) => new CSSUnitValue_js_1.default(value, 'vh');
    vmax = (value) => new CSSUnitValue_js_1.default(value, 'vmax');
    vmin = (value) => new CSSUnitValue_js_1.default(value, 'vmin');
    vw = (value) => new CSSUnitValue_js_1.default(value, 'vw');
    /**
     * Returns a Boolean indicating if the pair property-value, or the condition, given in parameter is supported.
     *
     * TODO: Always returns "true" for now, but it should probably be improved in the future.
     *
     * @param _condition Property name or condition.
     * @param [_value] Value when using property name.
     * @returns "true" if supported.
     */
    supports(_condition, _value) {
        return true;
    }
    /**
     * Escapes a value.
     *
     * @param value Value to escape.
     * @returns Escaped string.
     */
    escape(value) {
        return CSSEscaper_js_1.default.escape(value);
    }
}
exports.default = CSS;
//# sourceMappingURL=CSS.cjs.map