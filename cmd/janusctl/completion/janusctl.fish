# fish completion for janusctl - `janusctl completion fish`.
# Installed by the .deb in /usr/share/fish/vendor_completions.d/janusctl.fish;
# else: janusctl completion fish > ~/.config/fish/completions/janusctl.fish
function __janusctl_complete
    set -l tokens (commandline -opc) (commandline -ct)
    set -l out (janusctl __complete fish -- $tokens 2>/dev/null)
    for line in $out
        switch $line
            case :files
                __fish_complete_path (commandline -ct)
                return
            case :dirs
                __fish_complete_directories (commandline -ct)
                return
            case :nospace ''
            case '*'
                echo $line
        end
    end
end
complete -c janusctl -f -a '(__janusctl_complete)'
