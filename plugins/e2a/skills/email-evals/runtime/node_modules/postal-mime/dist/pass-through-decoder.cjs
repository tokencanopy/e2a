"use strict";
var __defProp = Object.defineProperty;
var __getOwnPropDesc = Object.getOwnPropertyDescriptor;
var __getOwnPropNames = Object.getOwnPropertyNames;
var __hasOwnProp = Object.prototype.hasOwnProperty;
var __export = (target, all) => {
  for (var name in all)
    __defProp(target, name, { get: all[name], enumerable: true });
};
var __copyProps = (to, from, except, desc) => {
  if (from && typeof from === "object" || typeof from === "function") {
    for (let key of __getOwnPropNames(from))
      if (!__hasOwnProp.call(to, key) && key !== except)
        __defProp(to, key, { get: () => from[key], enumerable: !(desc = __getOwnPropDesc(from, key)) || desc.enumerable });
  }
  return to;
};
var __toCommonJS = (mod) => __copyProps(__defProp({}, "__esModule", { value: true }), mod);
var pass_through_decoder_exports = {};
__export(pass_through_decoder_exports, {
  default: () => PassThroughDecoder
});
module.exports = __toCommonJS(pass_through_decoder_exports);
var import_decode_strings = require("./decode-strings.cjs");
class PassThroughDecoder {
  constructor() {
    this.chunks = [];
  }
  update(line) {
    this.chunks.push(line);
    this.chunks.push("\n");
  }
  finalize() {
    return (0, import_decode_strings.blobToArrayBuffer)(new Blob(this.chunks, { type: "application/octet-stream" }));
  }
}

// Make default export work naturally with require()
if (module.exports.default) {
    var defaultExport = module.exports.default;
    var namedExports = {};
    for (var key in module.exports) {
        if (key !== 'default' && key !== '__esModule') {
            namedExports[key] = module.exports[key];
        }
    }
    module.exports = defaultExport;
    Object.assign(module.exports, namedExports);
    // Preserve __esModule and .default for bundler/transpiler interop
    Object.defineProperty(module.exports, '__esModule', { value: true });
    module.exports.default = defaultExport;
}

