import 'react'

type MdProps = Record<string, any>

declare module 'react' {
  namespace JSX {
    interface IntrinsicElements {
      'md-circular-progress': MdProps
      'md-dialog': MdProps
      'md-fab': MdProps
      'md-filled-button': MdProps
      'md-filled-icon-button': MdProps
      'md-filled-tonal-button': MdProps
      'md-focus-ring': MdProps
      'md-icon-button': MdProps
      'md-linear-progress': MdProps
      'md-outlined-text-field': MdProps
      'md-primary-tab': MdProps
      'md-ripple': MdProps
      'md-tabs': MdProps
      'md-text-button': MdProps
    }
  }
}
