"use strict";
var __createBinding = (this && this.__createBinding) || (Object.create ? (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    var desc = Object.getOwnPropertyDescriptor(m, k);
    if (!desc || ("get" in desc ? !m.__esModule : desc.writable || desc.configurable)) {
      desc = { enumerable: true, get: function() { return m[k]; } };
    }
    Object.defineProperty(o, k2, desc);
}) : (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    o[k2] = m[k];
}));
var __setModuleDefault = (this && this.__setModuleDefault) || (Object.create ? (function(o, v) {
    Object.defineProperty(o, "default", { enumerable: true, value: v });
}) : function(o, v) {
    o["default"] = v;
});
var __importStar = (this && this.__importStar) || (function () {
    var ownKeys = function(o) {
        ownKeys = Object.getOwnPropertyNames || function (o) {
            var ar = [];
            for (var k in o) if (Object.prototype.hasOwnProperty.call(o, k)) ar[ar.length] = k;
            return ar;
        };
        return ownKeys(o);
    };
    return function (mod) {
        if (mod && mod.__esModule) return mod;
        var result = {};
        if (mod != null) for (var k = ownKeys(mod), i = 0; i < k.length; i++) if (k[i] !== "default") __createBinding(result, mod, k[i]);
        __setModuleDefault(result, mod);
        return result;
    };
})();
var __importDefault = (this && this.__importDefault) || function (mod) {
    return (mod && mod.__esModule) ? mod : { "default": mod };
};
Object.defineProperty(exports, "__esModule", { value: true });
const DetachedWindowAPI_js_1 = __importDefault(require("./DetachedWindowAPI.cjs"));
const BrowserWindow_js_1 = __importDefault(require("./BrowserWindow.cjs"));
const DetachedBrowser_js_1 = __importDefault(require("../browser/detached-browser/DetachedBrowser.cjs"));
const PropertySymbol = __importStar(require("../PropertySymbol.cjs"));
/**
 * Window.
 *
 * Reference:
 * https://developer.mozilla.org/en-US/docs/Web/API/Window.
 */
class Window extends BrowserWindow_js_1.default {
    // Detached Window API.
    happyDOM;
    /**
     * Constructor.
     *
     * @param [options] Options.
     * @param [options.width] Window width. Defaults to "1024".
     * @param [options.height] Window height. Defaults to "768".
     * @param [options.innerWidth] Inner width. Deprecated. Defaults to "1024".
     * @param [options.innerHeight] Inner height. Deprecated. Defaults to "768".
     * @param [options.url] URL.
     * @param [options.console] Console.
     * @param [options.settings] Settings.
     */
    constructor(options) {
        const browser = new DetachedBrowser_js_1.default(BrowserWindow_js_1.default, {
            console: options?.console,
            settings: options?.settings
        });
        const browserPage = browser.defaultContext.pages[0];
        const browserFrame = browserPage.mainFrame;
        if (options && (options.width || options.height || options.innerWidth || options.innerHeight)) {
            Object.assign(browserPage.viewport, {
                width: options.width || options.innerWidth || browserPage.viewport.width,
                height: options.height || options.innerHeight || browserPage.viewport.height
            });
        }
        super(browserFrame, {
            url: options?.url
        });
        browserFrame.window = this;
        if (browserFrame.page.context.browser[PropertySymbol.exceptionObserver]) {
            browserFrame.page.context.browser[PropertySymbol.exceptionObserver].observe(this);
        }
        this.happyDOM = new DetachedWindowAPI_js_1.default(browserFrame);
    }
}
exports.default = Window;
//# sourceMappingURL=Window.cjs.map