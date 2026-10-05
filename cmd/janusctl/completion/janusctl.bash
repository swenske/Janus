# bash completion for janusctl - `janusctl completion bash`.
# Installed by the .deb in /usr/share/bash-completion/completions/janusctl;
# else: source <(janusctl completion bash)  (in ~/.bashrc)
_janusctl() {
    local cur words cword
    if declare -F _get_comp_words_by_ref >/dev/null 2>&1; then
        # Keep "os:admin", "-file=NAME=PATH" whole.
        _get_comp_words_by_ref -n =: cur words cword
    else
        cur="${COMP_WORDS[COMP_CWORD]}"
        words=("${COMP_WORDS[@]}")
        cword=$COMP_CWORD
    fi
    local out line
    out="$(janusctl __complete bash -- "${words[@]:0:cword}" "$cur" 2>/dev/null)" || return
    COMPREPLY=()
    while IFS= read -r line; do
        case "$line" in
            :files) compopt -o default 2>/dev/null; COMPREPLY=(); return ;;
            :dirs) compopt -o dirnames 2>/dev/null; COMPREPLY=(); return ;;
            :nospace) compopt -o nospace 2>/dev/null ;;
            '') ;;
            *) COMPREPLY+=("$line") ;;
        esac
    done <<<"$out"
    if declare -F __ltrim_colon_completions >/dev/null 2>&1; then
        __ltrim_colon_completions "$cur"
    fi
}
complete -F _janusctl janusctl
