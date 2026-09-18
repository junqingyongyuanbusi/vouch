import { describe,it,expect } from "vitest"
import { add } from "./add.js"
describe("add",()=>{it("adds",()=>expect(add(1,2)).toBe(3));it("regression inject",()=>expect(add(1,2)).toBe(3))})
